package cli

import (
	"cmp"
	"context"
	"fmt"
	"net/url"
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
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/vendors"
)

// githubCopilotProvider is the registry name ListModels and pin validation key off.
const githubCopilotProvider = "github-copilot"

// configureProviderList prints every provider's credential state, or one provider's detail; json mirrors describe.
func configureProviderList(name, format string) int {
	if err := outfmt.Check(format); err != nil {
		fmt.Fprintln(os.Stderr, "claude-code: configure provider:", err)
		return 2
	}
	reg, err := providers.Load()
	if err != nil {
		return fail("%v", err)
	}
	if format == outfmt.JSON {
		return providerListJSON(reg, name)
	}
	if name == "" {
		printProviderTable(reg)
		return 0
	}
	p, err := reg.Get(name)
	if err != nil {
		return fail("%v", err)
	}
	fmt.Printf("provider  %s\nkind      %s\nauth      %s", p.Name, p.Kind, p.Auth.Type)
	if p.Auth.Env != "" {
		fmt.Printf(" (%s)", p.Auth.Env)
	}
	source, note := credSource(p)
	if note != "" {
		source += " (" + note + ")"
	}
	fmt.Printf("\ncred      %s\nroutes:\n", source)
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

// printProviderTable prints the providers grouped by family, columns sized to the widest name and kind; m2m rows hide.
func printProviderTable(reg *providers.Registry) {
	nameW, kindW, hidden := 0, 0, 0
	groups := map[string][]string{}
	for _, n := range reg.Names() {
		// An m2m row is a service principal that mints a u2m login from a cold start, not a session provider.
		if reg.Providers[n].Auth.AuthType == providers.AuthOAuthM2M {
			hidden++
			continue
		}
		nameW = max(nameW, len(n))
		kindW = max(kindW, len(reg.Providers[n].Kind))
		g := providerGroup(reg.Providers[n])
		groups[g] = append(groups[g], n)
	}
	first := true
	for _, g := range providerGroups {
		names := groups[g]
		if len(names) == 0 {
			continue
		}
		if !first {
			fmt.Println()
		}
		first = false
		fmt.Println(g)
		if g == groupDatabricks {
			slices.SortStableFunc(names, func(a, b string) int { return cmp.Compare(envSortKey(a), envSortKey(b)) })
		}
		for _, n := range names {
			p := reg.Providers[n]
			mark := " "
			if n == reg.Default {
				mark = "*"
			}
			cred, _ := credSource(p)
			fmt.Printf("%s %-*s %-*s %-12s %s\n", mark, nameW, n, kindW, p.Kind, cred, strings.Join(p.Dialects(), ","))
		}
	}
	if hidden > 0 || len(reg.Fallback) > 0 {
		fmt.Println()
	}
	if hidden > 0 {
		fmt.Printf("hidden: %d m2m service-principal row(s) that mint u2m logins (--list <name> or --format json shows them)\n", hidden)
	}
	if len(reg.Fallback) > 0 {
		fmt.Println("fallback:", strings.Join(reg.Fallback, " → "))
	}
}

const groupDatabricks = "databricks"

// providerGroups is the family order `configure provider --list` prints; opensource takes every other host.
var providerGroups = []string{"anthropic", "openai", "github", "google", groupDatabricks, "opensource"}

// providerGroup files p under a family: Databricks by preset or sealed auth, the rest by its route's host.
func providerGroup(p *providers.Provider) string {
	if p.Preset == providers.PresetDatabricks || p.Auth.AuthType != "" {
		return groupDatabricks
	}
	host := ""
	for _, d := range p.Dialects() {
		if r, ok := p.Route(d); ok {
			if u, err := url.Parse(r); err == nil {
				host = u.Hostname()
			}
			break
		}
	}
	for _, m := range []struct{ suffix, group string }{
		{"anthropic.com", "anthropic"}, {"openai.com", "openai"}, {"chatgpt.com", "openai"},
		{"githubcopilot.com", "github"}, {"github.com", "github"}, {"googleapis.com", "google"},
		{"databricks.com", groupDatabricks},
	} {
		if host == m.suffix || strings.HasSuffix(host, "."+m.suffix) {
			return m.group
		}
	}
	return "opensource"
}

// envSortKey orders Databricks rows bare env first (dev, qa, prod), then each prefix's -dev, -qa, -prod.
func envSortKey(name string) string {
	for i, env := range []string{"dev", "qa", "prod"} {
		if name == env {
			return fmt.Sprintf("0 %d", i)
		}
		if base, ok := strings.CutSuffix(name, "-"+env); ok {
			return fmt.Sprintf("1 %s %d", base, i)
		}
	}
	return "2 " + name
}

// credSource names where p's credential resolves from (vault|env|command|auth.file|passthrough|none|missing).
func credSource(p *providers.Provider) (source, note string) {
	st := secrets.Resolve(p)
	if st.Source == "none" && p.Auth.Type != providers.AuthNone {
		return "missing", st.Note
	}
	return st.Source, st.Note
}

// providerListEntry is the describe provider view plus where its credential resolves from.
type providerListEntry struct {
	DescribeProvider
	Group       string `json:"group"` // the family `--list` files it under (anthropic, openai, ... opensource)
	Source      string `json:"source"`
	Note        string `json:"note"`
	Host        string `json:"host,omitempty"` // non-secret: preset host, else BaseURL's scheme+host
	AccountID   string `json:"accountId,omitempty"`
	WorkspaceID string `json:"workspaceId,omitempty"`
}

// providerListJSON emits the describe provider view: all providers, or the one named, never a credential.
func providerListJSON(reg *providers.Registry, name string) int {
	d, err := describe(reg)
	if err != nil {
		return fail("configure provider: %v", err)
	}
	if name != "" {
		if _, err := reg.Get(name); err != nil {
			return fail("%v", err)
		}
	}
	list := make([]providerListEntry, 0, len(d.Providers.List))
	for _, dp := range d.Providers.List {
		if name != "" && dp.Name != name {
			continue
		}
		e := providerListEntry{DescribeProvider: dp}
		p := reg.Providers[dp.Name]
		e.Group = providerGroup(p)
		e.Source, e.Note = credSource(p)
		e.Host, e.AccountID, e.WorkspaceID = providers.AuthHost(p), p.Auth.AccountID, p.Auth.WorkspaceID
		list = append(list, e)
	}
	if name != "" {
		_ = outfmt.Write(os.Stdout, list[0])
		return 0
	}
	_ = outfmt.Write(os.Stdout, struct {
		Session  string              `json:"session"`
		Default  string              `json:"default"`
		Fallback []string            `json:"fallback,omitempty"`
		List     []providerListEntry `json:"list"`
	}{d.Providers.Session, d.Providers.Default, d.Providers.Fallback, list})
	return 0
}

var modelsPage = helptext.Page{
	Usage: "claude-code models [flags]",
	About: "Shows which model each tier slot (opus, sonnet, haiku, fable) resolves to on\n" +
		"the session provider. --pin and --unpin write the vault pins; --live lists the\n" +
		"provider's catalog.",
	Hint: "A HARNESS_<SLOT>_MODEL env var beats the vault pin.",
}

func cmdModels(args []string) int {
	fs := helptext.NewFlagSet("models", modelsPage)
	provider := fs.String("provider", "", "provider to resolve (default: pin, then registry default)")
	unpin := fs.Bool("unpin", false, "clear every model pin")
	live := fs.Bool("live", false, "list the models the provider serves (GET /models)")
	dryRun := fs.Bool("dry-run", false, "with --pin/--unpin: print the vault pins change, write nothing")
	format := outfmt.Flag(fs)
	var pins []string
	fs.Func("pin", "slot=[provider:]model, or all=[provider:]model (repeatable)", func(v string) error {
		pins = append(pins, v)
		return nil
	})
	args, err := outfmt.Parse(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
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
	if code, handled := applyModelPins(reg, *provider, *unpin, *dryRun, pins); handled {
		return code
	}
	sel, err := providers.Select(reg, *provider)
	if err != nil {
		return fail("%v", err)
	}
	return renderModelSelection(reg, sel, *format, *live)
}

func applyModelPins(reg *providers.Registry, provider string, unpin, dryRun bool, pins []string) (code int, handled bool) {
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
		kv[key] = qualifyPinSpec(reg, provider, spec)
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

// qualifyPinSpec stores a cross-provider "provider:model" pin when --provider names a provider other
// than the session's own, so the pin later resolves against the provider it was validated for, not
// whichever provider the session happens to be on (the bug: a bare model id pinned under --provider
// X but resolved under the session provider instead). Already-qualified specs pass through unchanged.
func qualifyPinSpec(reg *providers.Registry, providerFlag, spec string) string {
	if providerFlag == "" {
		return spec
	}
	if name, model, ok := strings.Cut(spec, ":"); ok {
		if _, found := reg.Providers[name]; found && model != "" {
			return spec
		}
	}
	session := cmp.Or(providers.Pin(providers.PinProvider), reg.Default)
	if providerFlag == session {
		return spec
	}
	return providerFlag + ":" + spec
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
	bin, err := paths.VendorBin("copilot")
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
	Task         string `json:"task,omitempty"`
	Ready        string `json:"ready,omitempty"`
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
			rows[i] = liveModel{ID: m.ID, Name: m.Name, UseCase: m.UseCase, Capabilities: m.Capabilities,
				Task: m.Task, Ready: m.Ready}
		}
		_ = outfmt.Write(os.Stdout, rows)
		return 0
	}
	if len(infos) > 0 && infos[0].Task != "" {
		printEndpointTable(infos)
		return 0
	}
	printModelTable(infos)
	return 0
}

// printEndpointTable prints a Databricks serving-endpoints listing as aligned name/task/ready columns.
func printEndpointTable(infos []providers.ModelInfo) {
	nameW, taskW := len("NAME"), len("TASK")
	for _, m := range infos {
		nameW, taskW = max(nameW, len(m.ID)), max(taskW, len(m.Task))
	}
	fmt.Printf("%-*s  %-*s  %s\n", nameW, "NAME", taskW, "TASK", "READY")
	for _, m := range infos {
		fmt.Println(strings.TrimRight(fmt.Sprintf("%-*s  %-*s  %s", nameW, m.ID, taskW, m.Task, m.Ready), " "))
	}
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

var switchPage = helptext.Page{
	Usage: "claude-code switch <provider> [flags]",
	About: "Pins the session provider in the vault; relaunch to apply. One launch only:\n" +
		"claude-code --provider <p>.",
	Hint: "claude-code configure provider --list shows each provider's credential state.",
}

func cmdSwitch(args []string) int {
	dryRun, args := extractBool(args, "dry-run")
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		switchUsage()
		return 0
	}
	if len(args) != 1 {
		switchUsage()
		return 2
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

// switchUsage prints switch's page; its one flag is pulled out by extractBool, so this FlagSet only renders it.
func switchUsage() {
	fs := helptext.NewFlagSet("switch", switchPage)
	fs.Bool("dry-run", false, "print the vault pins change, write nothing")
	fs.Usage()
}

var resolvePage = helptext.Page{
	Usage: "claude-code resolve [flags]",
	About: "Resolves and prints the session provider's credential: the apiKeyHelper\n" +
		"Claude Code calls. --router prints the loopback router's client secret instead.",
}

// cmdToken is the apiKeyHelper: --router prints the loopback router's client secret instead.
func cmdToken(args []string) int {
	fs := helptext.NewFlagSet("resolve", resolvePage)
	provider := fs.String("provider", "", "provider name")
	viaRouter := fs.Bool("router", false, "print the loopback router client secret")
	if code, done := helptext.Parse(fs, args); done {
		return code
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
	diff, err := providers.PinDiff(kv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "claude-code:", err)
		return 1
	}
	if len(diff) == 0 {
		fmt.Println("no change to the vault pins")
		return 0
	}
	fmt.Println("would write the vault pins:")
	for _, l := range diff {
		fmt.Println("  " + l)
	}
	return 0
}

// copilotCatalogOptions lists models as the account copilot runs as (COPILOT_GITHUB_USER), not its stored login.
func copilotCatalogOptions(bin string) copilotsdk.Options {
	return copilotsdk.CatalogOptions(bin, vendors.CopilotToken())
}
