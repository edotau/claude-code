package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
)

func cmdProviders(args []string) int {
	reg, err := providers.Load()
	if err != nil {
		return fail("%v", err)
	}
	if len(args) == 0 {
		for _, n := range reg.Names() {
			p := reg.Providers[n]
			mark := " "
			if n == reg.Default {
				mark = "*"
			}
			cred := "cred:missing"
			if providers.CredentialPresent(p) {
				cred = "cred:ok"
			}
			fmt.Printf("%s %-13s %-10s %-12s %s\n", mark, n, p.Kind, cred, strings.Join(p.Dialects(), ","))
		}
		if len(reg.Fallback) > 0 {
			fmt.Println("fallback:", strings.Join(reg.Fallback, " → "))
		}
		return 0
	}
	p, err := reg.Get(args[0])
	if err != nil {
		return fail("%v", err)
	}
	fmt.Printf("provider  %s\nkind      %s\nauth      %s", p.Name, p.Kind, p.Auth.Type)
	if p.Auth.Env != "" {
		fmt.Printf(" (%s)", p.Auth.Env)
	}
	fmt.Printf("\ncred      %v\nroutes:\n", providers.CredentialPresent(p))
	for _, d := range p.Dialects() {
		r, _ := p.Route(d)
		fmt.Printf("  %-10s %s\n", d, r)
	}
	fmt.Println("models:")
	for _, s := range models.Slots {
		if id := p.Models[s]; id != "" {
			fmt.Printf("  %-10s %s\n", s, id)
		}
	}
	return 0
}

func cmdModels(args []string) int {
	fs := flag.NewFlagSet("models", flag.ContinueOnError)
	provider := fs.String("provider", "", "provider to resolve (default: pin, then registry default)")
	unpin := fs.Bool("unpin", false, "clear every model pin")
	live := fs.Bool("live", false, "list the models the provider serves (GET /models)")
	var pins []string
	fs.Func("pin", "slot=[provider:]model, or all=[provider:]model (repeatable)", func(v string) error {
		pins = append(pins, v)
		return nil
	})
	if err := fs.Parse(args); err != nil {
		return 2
	}
	reg, err := providers.Load()
	if err != nil {
		return fail("%v", err)
	}
	if *unpin || len(pins) > 0 {
		kv := map[string]string{}
		if *unpin {
			kv[providers.PinModel] = ""
			for _, s := range models.Slots {
				kv[providers.SlotPin(s)] = ""
			}
		}
		for _, p := range pins {
			slot, spec, ok := strings.Cut(p, "=")
			if !ok {
				return fail("--pin wants slot=model, got %q", p)
			}
			key := providers.PinModel
			if slot != "all" {
				if !validSlot(slot) {
					return fail("unknown slot %q (opus|sonnet|haiku|fable|all)", slot)
				}
				key = providers.SlotPin(slot)
			}
			kv[key] = spec
		}
		if err := providers.SetPins(kv); err != nil {
			return fail("%v", err)
		}
	}
	sel, err := providers.Select(reg, *provider)
	if err != nil {
		return fail("%v", err)
	}
	if *live {
		return listLive(sel.Provider)
	}
	fmt.Printf("provider  %s   router:%v\n", sel.Provider.Name, sel.NeedsRouter(reg))
	for _, s := range models.Slots {
		if t, ok := sel.Slots[s]; ok {
			fmt.Printf("  %-7s %-40s → %s\n", s, sel.ClientModel(s), t.Spec())
		}
	}
	return 0
}

func validSlot(s string) bool {
	for _, x := range models.Slots {
		if x == s {
			return true
		}
	}
	return false
}

// listLive GETs the provider's model catalog on whichever route it has (Anthropic /v1/models, else OpenAI /models).
func listLive(p *providers.Provider) int {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	url := ""
	if r, ok := p.Route(providers.DialectAnthropic); ok {
		url = r + "/v1/models"
	} else if r, ok := p.Route(providers.DialectOpenAI); ok {
		url = r + "/models"
	} else {
		return fail("%s has no route that lists models", p.Name)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err := providers.Authorize(ctx, p, req.Header); err != nil {
		return fail("%v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fail("%v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return fail("GET %s: %s: %s", url, resp.Status, strings.TrimSpace(string(body)))
	}
	var doc struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return fail("decode %s: %v", url, err)
	}
	for _, m := range doc.Data {
		fmt.Println(m.ID)
	}
	return 0
}

func cmdUse(args []string) int {
	if len(args) != 1 {
		return fail("usage: claude-code use <provider>")
	}
	reg, err := providers.Load()
	if err != nil {
		return fail("%v", err)
	}
	if _, err := reg.Get(args[0]); err != nil {
		return fail("%v", err)
	}
	if err := providers.SetPins(map[string]string{providers.PinProvider: args[0]}); err != nil {
		return fail("%v", err)
	}
	fmt.Printf("provider pinned: %s (relaunch to apply)\n", args[0])
	return 0
}

// cmdToken is the apiKeyHelper: --router prints the loopback router's client secret instead.
func cmdToken(args []string) int {
	fs := flag.NewFlagSet("token", flag.ContinueOnError)
	provider := fs.String("provider", "", "provider name")
	viaRouter := fs.Bool("router", false, "print the loopback router client secret")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *viaRouter {
		s, err := routerClientSecret()
		if err != nil {
			return fail("%v", err)
		}
		fmt.Println(s)
		return 0
	}
	reg, err := providers.Load()
	if err != nil {
		return fail("%v", err)
	}
	sel, err := providers.Select(reg, *provider)
	if err != nil {
		return fail("%v", err)
	}
	tok, err := providers.Credential(context.Background(), sel.Provider)
	if err != nil {
		return fail("%v", err)
	}
	fmt.Fprintln(os.Stdout, tok)
	return 0
}
