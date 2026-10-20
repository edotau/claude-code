package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/copilotsdk"
	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/outfmt"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vendors"
)

// githubCopilotProvider is the registry name ListModels and pin validation key off.
const githubCopilotProvider = "github-copilot"

func cmdProviders(args []string) int {
	fs := flag.NewFlagSet("profile", flag.ContinueOnError)
	fs.Usage = func() {
		helptext.Fprint(fs.Output(), "Usage of profile:\n")
		helptext.Defaults(fs)
	}
	format := outfmt.Flag(fs)
	args, err := outfmt.Parse(fs, args)
	if err != nil {
		return 2
	}
	if err := outfmt.Check(*format); err != nil {
		fmt.Fprintln(os.Stderr, "claude-code: profile:", err)
		return 2
	}
	var reg *providers.Registry
	reg, err = providers.Load()
	if err != nil {
		return fail("%v", err)
	}
	if *format == outfmt.JSON {
		return profileJSON(reg, args)
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

// profileJSON emits the describe provider view: all providers, or the one named, never a credential.
func profileJSON(reg *providers.Registry, args []string) int {
	d, err := describe(reg)
	if err != nil {
		return fail("profile: %v", err)
	}
	if len(args) == 0 {
		_ = outfmt.Write(os.Stdout, d.Providers)
		return 0
	}
	if _, err := reg.Get(args[0]); err != nil {
		return fail("%v", err)
	}
	for _, p := range d.Providers.List {
		if p.Name == args[0] {
			_ = outfmt.Write(os.Stdout, p)
		}
	}
	return 0
}

func cmdModels(args []string) int {
	fs := flag.NewFlagSet("models", flag.ContinueOnError)
	fs.Usage = func() {
		helptext.Fprint(fs.Output(), "Usage of models:\n")
		helptext.Defaults(fs)
	}
	provider := fs.String("provider", "", "provider to resolve (default: pin, then registry default)")
	unpin := fs.Bool("unpin", false, "clear every model pin")
	live := fs.Bool("live", false, "list the models the provider serves (GET /models)")
	dryRun := fs.Bool("dry-run", false, "with --pin/--unpin: print the pin-file change, write nothing")
	format := outfmt.Flag(fs)
	var pins []string
	fs.Func("pin", "slot=[provider:]model, or all=[provider:]model (repeatable)", func(v string) error {
		pins = append(pins, v)
		return nil
	})
	args, err := outfmt.Parse(fs, args)
	if err != nil {
		return 2
	}
	if len(args) > 0 {
		fmt.Fprintf(os.Stderr, "claude-code: models: unexpected argument %q\n", args[0])
		return 2
	}
	if err := outfmt.Check(*format); err != nil {
		fmt.Fprintln(os.Stderr, "claude-code: models:", err)
		return 2
	}
	reg, err := providers.Load()
	if err != nil {
		return fail("%v", err)
	}
	if code, handled := applyModelPins(*provider, *unpin, *dryRun, pins); handled {
		return code
	}
	sel, err := providers.Select(reg, *provider)
	if err != nil {
		return fail("%v", err)
	}
	return renderModelSelection(reg, sel, *format, *live)
}

func applyModelPins(provider string, unpin, dryRun bool, pins []string) (code int, handled bool) {
	if !unpin && len(pins) == 0 {
		return 0, false
	}
	kv := map[string]string{}
	if unpin {
		kv[providers.PinModel] = ""
		for _, s := range models.Slots {
			kv[providers.SlotPin(s)] = ""
		}
	}
	for _, p := range pins {
		slot, spec, ok := strings.Cut(p, "=")
		if !ok {
			return fail("--pin wants slot=model, got %q", p), true
		}
		key := providers.PinModel
		if slot != "all" {
			if !slices.Contains(models.Slots, slot) {
				return fail("unknown slot %q (opus|sonnet|haiku|fable|all)", slot), true
			}
			key = providers.SlotPin(slot)
		}
		kv[key] = spec
	}
	warn, err := validateCopilotPins(provider, pins)
	if err != nil {
		return fail("%v", err), true
	}
	if warn != "" {
		fmt.Fprintln(os.Stderr, "claude-code: models:", warn)
	}
	if dryRun {
		return printPinDiff(kv), true
	}
	if err := providers.SetPins(kv); err != nil {
		return fail("%v", err), true
	}
	return 0, false
}

func renderModelSelection(reg *providers.Registry, sel providers.Selection, format string, live bool) int {
	if live {
		return listLive(reg, sel.Provider, format)
	}
	if format == "json" {
		slots := map[string]string{}
		for _, s := range models.Slots {
			if t, ok := sel.Slots[s]; ok {
				slots[s] = t.Spec()
			}
		}
		if err := outfmt.Write(os.Stdout, map[string]any{"provider": sel.Provider.Name, "router": sel.NeedsRouter(reg), "slots": slots}); err != nil {
			return fail("models: %v", err)
		}
		return 0
	}
	fmt.Printf("provider  %s   router:%v\n", sel.Provider.Name, sel.NeedsRouter(reg))
	for _, s := range models.Slots {
		if t, ok := sel.Slots[s]; ok {
			fmt.Printf("  %-7s %-40s → %s\n", s, sel.ClientModel(s), t.Spec())
		}
	}
	return 0
}

// validateCopilotPins checks each pin whose effective provider is github-copilot against its live catalog.
// Returns a warning when the catalog itself is unreachable (pin still written by the caller); an unknown
// id is a hard error naming near matches, which the caller must surface before writing anything.
func validateCopilotPins(providerFlag string, pins []string) (warn string, err error) {
	fallback := providerFlag
	if fallback == "" {
		fallback = providers.Pin(providers.PinProvider)
	}
	var ids []string
	var fetched bool
	for _, p := range pins {
		_, spec, ok := strings.Cut(p, "=")
		if !ok {
			continue
		}
		name, model, hasProvider := strings.Cut(spec, ":")
		if !hasProvider || model == "" {
			name, model = fallback, spec
		}
		if name != githubCopilotProvider {
			continue
		}
		if !fetched {
			fetched = true
			ids, err = copilotModelIDs()
			if err != nil {
				return fmt.Sprintf("could not verify against the live github-copilot catalog (pin written anyway): %v", err), nil
			}
		}
		if !slices.Contains(ids, model) {
			if near := nearMatches(model, ids); len(near) > 0 {
				return "", fmt.Errorf("%q is not a github-copilot model; did you mean: %s?", model, strings.Join(near, ", "))
			}
			return "", fmt.Errorf("%q is not a github-copilot model (run `claude-code models --live --provider github-copilot` to see the catalog)", model)
		}
	}
	return "", nil
}

// nearMatches is a cheap substring match, not full edit distance: enough to catch a typo'd suffix/prefix.
func nearMatches(want string, ids []string) []string {
	var out []string
	for _, id := range ids {
		if strings.Contains(id, want) || strings.Contains(want, id) {
			out = append(out, id)
		}
	}
	return out
}

// copilotModelIDs resolves the copilot CLI and lists its live catalog via the SDK (the harness never reads a token).
func copilotModelIDs() ([]string, error) {
	bin, err := paths.LookPathReal("copilot")
	if err != nil {
		return nil, fmt.Errorf("copilot CLI not found: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	list, err := copilotsdk.ListModels(ctx, copilotCatalogOptions(bin))
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(list))
	for i, m := range list {
		ids[i] = m.ID
	}
	return ids, nil
}

// liveModel is the JSON row for one catalog entry; the text view prints the same fields as an aligned table.
type liveModel struct {
	ID           string `json:"id"`
	Name         string `json:"name,omitempty"`
	UseCase      string `json:"use_case"`
	Capabilities string `json:"capabilities,omitempty"`
}

// listLive prints the provider's live model catalog via providers.ListModels (github-copilot's own SDK,
// every other provider's plain /models GET). A catalog that can't be fetched live falls back to a known,
// offline list for the two providers that have one (subscription's registry models, chatgpt's codex cache).
func listLive(reg *providers.Registry, p *providers.Provider, format string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	infos, err := providers.ListModels(ctx, reg, p)
	note := ""
	if err != nil {
		if infos, note, err = providers.FallbackModels(p, err); err != nil {
			return fail("%v", err)
		}
	}
	if note != "" {
		fmt.Fprintln(os.Stderr, "claude-code: models:", note)
	}
	if format == outfmt.JSON {
		rows := make([]liveModel, len(infos))
		for i, m := range infos {
			rows[i] = liveModel{ID: m.ID, Name: m.Name, UseCase: m.UseCase, Capabilities: m.Capabilities}
		}
		_ = outfmt.Write(os.Stdout, rows)
		return 0
	}
	printModelTable(infos)
	return 0
}

// printModelTable prints an aligned ID/Model/Use case/Capabilities table; widths are computed from the
// rows, not fixed, and a gemini "models/" id prefix is dropped for display only (JSON keeps the raw id).
func printModelTable(infos []providers.ModelInfo) {
	idW, nameW, caseW, capW := len("ID"), len("Model"), len("Use case"), len("Capabilities")
	ids := make([]string, len(infos))
	for i, m := range infos {
		ids[i] = strings.TrimPrefix(m.ID, "models/")
		idW = max(idW, len(ids[i]))
		nameW = max(nameW, len(m.Name))
		caseW = max(caseW, len(m.UseCase))
		capW = max(capW, len(m.Capabilities))
	}
	fmt.Println(strings.TrimRight(fmt.Sprintf("%-*s  %-*s  %-*s  %-*s", idW, "ID", nameW, "Model", caseW, "Use case", capW, "Capabilities"), " "))
	for i, m := range infos {
		fmt.Println(strings.TrimRight(fmt.Sprintf("%-*s  %-*s  %-*s  %-*s", idW, ids[i], nameW, m.Name, caseW, m.UseCase, capW, m.Capabilities), " "))
	}
}

func cmdUse(args []string) int {
	dryRun, args := extractBool(args, "dry-run")
	if len(args) != 1 || args[0] == "-h" || args[0] == "--help" {
		helptext.Fprint(os.Stderr, "usage: claude-code switch <provider> [--dry-run]\n")
		return 1
	}
	reg, err := providers.Load()
	if err != nil {
		return fail("%v", err)
	}
	if _, err := reg.Get(args[0]); err != nil {
		return fail("%v", err)
	}
	if dryRun {
		return printPinDiff(map[string]string{providers.PinProvider: args[0]})
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
	fs.Usage = func() {
		helptext.Fprint(fs.Output(), "Usage of token:\n")
		helptext.Defaults(fs)
	}
	provider := fs.String("provider", "", "provider name")
	viaRouter := fs.Bool("router", false, "print the loopback router client secret")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *viaRouter {
		s, err := router.ClientSecret()
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

// printPinDiff is the shared --dry-run body for switch and models: the change, or that there is none.
func printPinDiff(kv map[string]string) int {
	diff := providers.PinDiff(kv)
	if len(diff) == 0 {
		fmt.Println("no change to", paths.PinFile())
		return 0
	}
	fmt.Println("would write", paths.PinFile()+":")
	for _, l := range diff {
		fmt.Println("  " + l)
	}
	return 0
}

// copilotCatalogOptions lists models as the account copilot runs as (COPILOT_GITHUB_USER), not its stored login.
func copilotCatalogOptions(bin string) copilotsdk.Options {
	if tok := vendors.CopilotToken(); tok != "" {
		return copilotsdk.Options{CLIPath: bin, GitHubToken: tok}
	}
	return copilotsdk.Options{CLIPath: bin, UseLoggedInUser: true}
}
