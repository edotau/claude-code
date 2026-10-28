package databricks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/outfmt"
	"github.com/edotau/claude-code/internal/providers"
)

const (
	probeTimeout = 45 * time.Second
	budgetDenied = "budget denied"
)

var checkAllPage = helptext.Page{
	Usage: "claude-code databricks check --all [flags]",
	About: "One tiny Anthropic call per profile and wire (serving, ai-gateway);\n" +
		"prints a status per cell, never a token. Exit 1 when a cell fails\n" +
		"for any reason other than a budget denial.",
}

// probeClient is the HTTP client for probes, a var so tests point it at httptest.
var probeClient = &http.Client{Timeout: probeTimeout}

// checkAll is `databricks check --all`: the profile x wire matrix (one profile with --profile).
func checkAll(args []string) int {
	fs := helptext.NewFlagSet("databricks check --all", checkAllPage)
	model := fs.String("model", aliasID(models.Haiku), "canonical model id to probe")
	only := fs.String("profile", "", "probe only this Databricks profile (default: all)")
	fs.StringVar(only, "p", "", "shorthand for --profile")
	if rest, err := outfmt.Parse(fs, args); err != nil {
		return helptext.ExitCode(err)
	} else if len(rest) > 0 {
		fs.Usage()
		return 2
	}
	reg, err := providers.Load()
	if err != nil {
		return fail("check: %v", err)
	}
	wires := []string{providers.WireServing, providers.WireAIGateway}
	names := providers.DatabricksProfiles(reg)
	if *only != "" {
		names = []string{*only}
	}
	fmt.Printf("%-24s %-24s %s\n", "PROFILE", "SERVING", "AI-GATEWAY")
	rc := 0
	for _, name := range names {
		cells := checkProfile(reg.Providers[name], name, *model, wires)
		for _, c := range cells {
			if c != "ok" && c != budgetDenied {
				rc = 1
			}
		}
		fmt.Printf("%-24s %-24s %s\n", name, cells[0], cells[1])
	}
	return rc
}

// checkProfile mints the row's token once and probes every wire with it.
func checkProfile(row *providers.Provider, name, model string, wires []string) []string {
	cells := make([]string, len(wires))
	fill := func(s string) []string {
		for i := range cells {
			cells[i] = s
		}
		return cells
	}
	if row == nil {
		return fill("no row")
	}
	host := providers.AuthHost(row)
	if host == "" {
		return fill("no https host")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*probeTimeout)
	defer cancel()
	tok, err := providers.Credential(ctx, row)
	if err != nil {
		return fill("no token: " + clip(strings.TrimPrefix(err.Error(), name+": "), 40))
	}
	for i, w := range wires {
		p := row
		if w != row.Options.Wire && (w != providers.WireServing || row.Options.Wire != "") {
			if p, _, err = synth(name, host, w); err != nil {
				cells[i] = clip(err.Error(), 40)
				continue
			}
		}
		cells[i] = probe(ctx, p, tok, model)
	}
	return cells
}

// probe sends one max_tokens=1 message on p's Anthropic route and classifies the reply.
func probe(ctx context.Context, p *providers.Provider, tok, model string) string {
	base, ok := p.Route(providers.ProtocolAnthropic)
	if !ok {
		return "no anthropic route"
	}
	body, _ := json.Marshal(map[string]any{"model": p.WireModel(model), "max_tokens": 1,
		"messages": []map[string]string{{"role": "user", "content": "ping"}}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/v1/messages",
		bytes.NewReader(body))
	if err != nil {
		return "bad request"
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	for k, v := range p.Headers {
		req.Header.Set(k, v)
	}
	resp, err := probeClient.Do(req)
	if err != nil {
		return "network error"
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return classify(resp.StatusCode, strings.ReplaceAll(string(raw), tok, "***"))
}

// classify names a probe outcome: ok, budget denied (403 mentioning budget), else HTTP <code>: <message>.
func classify(status int, body string) string {
	if status >= 200 && status < 300 {
		return "ok"
	}
	if status == http.StatusForbidden && strings.Contains(strings.ToLower(body), "budget") {
		return budgetDenied
	}
	var e struct {
		Message string `json:"message"`
		Error   struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	msg := strings.TrimSpace(body)
	if json.Unmarshal([]byte(body), &e) == nil {
		msg = strings.TrimSpace(e.Error.Message + " " + e.Message)
	}
	if msg == "" {
		return fmt.Sprintf("HTTP %d", status)
	}
	return clip(fmt.Sprintf("HTTP %d: %s", status, msg), 48)
}

// clip cuts s to n runes on one line.
func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
