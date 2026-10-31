package cli

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/preflight"
	"github.com/edotau/claude-code/internal/copilotsdk"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// configureProviderList prints every provider's credential state, or one provider's detail; json mirrors describe.
func configureProviderList(name, format string) int {
	if err := helptext.CheckFormat(format); err != nil {
		return badFormat("configure provider", err)
	}
	reg, err := providers.Load()
	if err != nil {
		return fail("%v", err)
	}
	if format == helptext.JSON {
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
	fmt.Printf("profile   %s\nprotocol  %s\nauth      %s", p.Name, p.Protocol, p.Auth.Method())
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

// printProviderTable prints one row per provider, then one per agent CLI: the profiles whose credential
// resolves now (m2m service principals excluded) and the auth methods they use.
func printProviderTable(reg *providers.Registry) {
	groups := map[string][]string{}
	modes := map[string][]string{}
	for _, n := range reg.Names() {
		p := reg.Providers[n]
		if cred, _ := credSource(p); cred == "missing" {
			continue
		}
		g := p.Provider
		modes[g] = appendUnique(modes[g], providerMode(p))
		if p.Auth.Method() == providers.AuthM2M {
			continue // a service principal mints oauth logins from a cold start; it is an auth, not a profile
		}
		groups[g] = append(groups[g], n)
	}
	var rows [][3]string
	var ready []*providers.Provider // provider order, so the agent rows read like the provider rows
	for _, g := range providerGroups {
		if len(groups[g]) > 0 {
			names := sortProfiles(g, groups[g])
			rows = append(rows, [3]string{g, familyProfiles(reg, names), strings.Join(modes[g], ", ")})
			for _, n := range names {
				ready = append(ready, reg.Providers[n])
			}
		}
	}
	rows = append(rows, [3]string{})
	for _, v := range agent.Names() {
		var names, vm []string
		for _, p := range ready {
			if agent.CanServe(v, p) {
				names, vm = append(names, p.Name), appendUnique(vm, providerMode(p))
			}
		}
		if len(names) > 0 {
			rows = append(rows, [3]string{v, familyProfiles(reg, names), strings.Join(vm, ", ")})
		}
	}
	w0, w1 := len("PROVIDERS"), len("PROFILES")
	for _, r := range rows {
		w0, w1 = max(w0, len(r[0])), max(w1, len(r[1]))
	}
	fmt.Printf("%-*s  %-*s  %s\n", w0, "PROVIDERS", w1, "PROFILES", "AUTH")
	for _, r := range rows {
		fmt.Println(strings.TrimRight(fmt.Sprintf("%-*s  %-*s  %s", w0, r[0], w1, r[1], r[2]), " "))
	}
	if len(reg.Fallback) > 0 {
		fmt.Println()
		fmt.Println("fallback:", strings.Join(reg.Fallback, " → "))
	}
}

// providerMode is how p authenticates: oauth, m2m, pat, key, cli (a CLI's own login), or "-".
func providerMode(p *providers.Provider) string {
	switch m := p.Auth.Method(); {
	case p.Preset == providers.PresetDatabricks && m == providers.AuthKey:
		return providers.AuthOAuth
	case p.Auth.Login != "":
		return providers.AuthCLI
	case m == providers.AuthNone || m == "":
		return "-"
	default:
		return m
	}
}

// familyProfiles joins names, marking the session default with *.
func familyProfiles(reg *providers.Registry, names []string) string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = n
		if n == reg.Default {
			out[i] = "*" + n
		}
	}
	return strings.Join(out, ", ")
}

// sortProfiles keeps Databricks rows in env order (dev, qa, prod); other groups stay alphabetical.
func sortProfiles(group string, names []string) []string {
	if group == providers.GroupDatabricks {
		slices.SortStableFunc(names, func(a, b string) int { return cmp.Compare(envSortKey(a), envSortKey(b)) })
	}
	return names
}

func appendUnique(xs []string, x string) []string {
	if slices.Contains(xs, x) {
		return xs
	}
	return append(xs, x)
}

// providerGroups is the family order `configure provider --list` prints; local takes every other host.
var providerGroups = []string{"anthropic", "openai", "github", "google", providers.GroupDatabricks, "nvidia", "local"}

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

// credSource names where p's credential resolves from (vault|env|command|auth.file|cli|none|missing).
func credSource(p *providers.Provider) (source, note string) {
	st := secrets.Resolve(p)
	if st.Source == "none" && p.Auth.Method() != providers.AuthNone {
		return "missing", st.Note
	}
	return st.Source, st.Note
}

// providerListEntry is the describe provider view plus where its credential resolves from.
type providerListEntry struct {
	DescribeProvider
	Group       string `json:"group"` // the family `--list` files it under (anthropic, openai, ... local)
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
		e.Group = p.Provider
		e.Source, e.Note = credSource(p)
		e.Host, e.AccountID, e.WorkspaceID = providers.AuthHost(p), p.Auth.AccountID, p.Auth.WorkspaceID
		list = append(list, e)
	}
	if name != "" {
		_ = helptext.WriteJSON(os.Stdout, list[0])
		return 0
	}
	_ = helptext.WriteJSON(os.Stdout, struct {
		Session  string              `json:"session"`
		Default  string              `json:"default"`
		Fallback []string            `json:"fallback,omitempty"`
		List     []providerListEntry `json:"list"`
	}{d.Providers.Session, d.Providers.Default, d.Providers.Fallback, list})
	return 0
}

var modelsPage = helptext.Page{
	Usage: "claude-code models [flags]",
	About: "Shows which model each alias (opus, sonnet, haiku, fable) resolves to on\n" +
		"the session profile. --pin and --unpin write the vault pins; --live lists the\n" +
		"profile's catalog.",
	Hint: "A CLAUDE_<ALIAS>_MODEL vault pin beats the profile's own model.",
}

func cmdModels(args []string) int {
	fs := helptext.NewFlagSet("models", modelsPage)
	provider := fs.String("profile", "", "profile to resolve (default: pin, then registry default)")
	unpin := fs.Bool("unpin", false, "clear every model pin")
	live := fs.Bool("live", false, "list the models the profile serves (GET /models)")
	dryRun := fs.Bool("dry-run", false, "with --pin/--unpin: print the vault pins change, write nothing")
	format := helptext.FormatFlag(fs)
	var pins []string
	fs.Func("pin", "alias=model, or all=model (repeatable)", func(v string) error {
		pins = append(pins, v)
		return nil
	})
	args, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(args) > 0 {
		fmt.Fprintf(os.Stderr, "claude-code: models: unexpected argument %q\n", args[0])
		return 2
	}
	if err := helptext.CheckFormat(*format); err != nil {
		return badFormat("models", err)
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
			return fail("--pin wants alias=model, got %q", p), true
		}
		key := providers.PinModel
		if slot != "all" {
			if !slices.Contains(models.Slots, slot) {
				return fail("unknown alias %q (opus|sonnet|haiku|fable|all)", slot), true
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

// qualifyPinSpec stores a cross-profile "profile:model" pin when --profile names a profile other than the
// session's own, so the pin later resolves against the profile it was validated for. Qualified specs pass through.
func qualifyPinSpec(reg *providers.Registry, providerFlag, spec string) string {
	if providerFlag == "" {
		return spec
	}
	if name, model, ok := strings.Cut(spec, ":"); ok {
		if _, found := reg.Providers[name]; found && model != "" {
			return spec
		}
	}
	session := cmp.Or(providers.SessionProfile(), providers.Pin(providers.PinProfile), reg.Default)
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
		byAlias := map[string]string{}
		for _, s := range models.Slots {
			if t, ok := sel.Slots[s]; ok {
				byAlias[s] = t.Spec()
			}
		}
		if err := helptext.WriteJSON(os.Stdout, map[string]any{"profile": sel.Provider.Name, "proxy": sel.NeedsRouter(reg), "models": byAlias}); err != nil {
			return fail("models: %v", err)
		}
		return 0
	}
	fmt.Printf("profile  %s   proxy:%v\n", sel.Provider.Name, sel.NeedsRouter(reg))
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
		fallback = cmp.Or(providers.SessionProfile(), providers.Pin(providers.PinProfile))
	}
	reg, rerr := providers.Load()
	if rerr != nil {
		return "", nil
	}
	var ids []string
	var fetched, catalogDown bool
	var firstWarn string // the first unverifiable-pin warning; later pins are still checked
	served := map[string][]string{}
	for _, p := range pins {
		_, spec, ok := strings.Cut(p, "=")
		if !ok {
			continue
		}
		name, model, hasProvider := strings.Cut(spec, ":")
		if !hasProvider || model == "" {
			name, model = fallback, spec
		}
		if dp := reg.Providers[name]; dp != nil && dp.Preset == providers.PresetDatabricks {
			w, derr := validateDatabricksCopilotPin(reg, name, model, served)
			if derr != nil {
				return "", derr
			}
			firstWarn = cmp.Or(firstWarn, w)
			continue
		}
		gh, gerr := reg.Get(name)
		if gerr != nil || gh.Provider != "github" {
			continue
		}
		if !fetched {
			fetched = true
			ids, err = copilotModelIDs()
			if err != nil {
				catalogDown = true
				firstWarn = cmp.Or(firstWarn, fmt.Sprintf("could not verify against the live github-copilot catalog (pin written anyway): %v", err))
			}
		}
		if catalogDown {
			continue
		}
		if !slices.Contains(ids, model) {
			if near := nearMatches(model, ids); len(near) > 0 {
				return "", fmt.Errorf("%q is not a github-copilot model; did you mean: %s?", model, strings.Join(near, ", "))
			}
			return "", fmt.Errorf("%q is not a github-copilot model (run `claude-code models --live --profile %s` to see the catalog)", model, gh.Name)
		}
	}
	return firstWarn, nil
}

// validateDatabricksCopilotPin checks model against name's serving endpoints; served caches each profile's ids.
func validateDatabricksCopilotPin(
	reg *providers.Registry, name, model string, served map[string][]string,
) (string, error) {
	ids, ok := served[name]
	if !ok {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		cat, err := agent.CopilotModelSource(ctx, reg, name)
		cancel()
		if err != nil {
			return fmt.Sprintf("could not verify against %s serving endpoints (pin written anyway): %v", name, err), nil
		}
		for _, r := range cat.Rows {
			ids = append(ids, r.ID)
		}
		served[name] = ids
	}
	want := []string{model, models.DatabricksPrefix + model}
	if c, _, ok := models.Resolve(model); ok {
		want = append(want, c)
	}
	for _, id := range ids {
		c, _, _ := models.Resolve(id)
		if slices.Contains(want, id) || (c != "" && slices.Contains(want, c)) {
			return "", nil
		}
	}
	hint := fmt.Sprintf("run `claude-code models --live --profile %s` to see the endpoints", name)
	if near := nearMatches(model, ids); len(near) > 0 {
		return "", fmt.Errorf("%q is not a %s serving endpoint; did you mean: %s? (%s)",
			model, name, strings.Join(near, ", "), hint)
	}
	return "", fmt.Errorf("%q is not a %s serving endpoint (%s)", model, name, hint)
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
	Gateway      bool   `json:"gateway,omitempty"`
}

// listLive prints the provider's live model catalog via providers.ListModels (github-copilot's own SDK,
// every other provider's plain /models GET). A catalog that can't be fetched live falls back to a known,
// offline list for the two profiles that have one (max's registry models, chatgpt's codex cache).
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
	gw, gwErr := providers.GatewayModels(ctx, p)
	if gwErr != nil {
		fmt.Fprintln(os.Stderr, "claude-code: models: ai gateway listing unavailable:", gwErr)
	}
	if format == helptext.JSON {
		rows := make([]liveModel, 0, len(infos)+len(gw))
		for _, m := range append(slices.Clone(infos), gw...) {
			rows = append(rows, liveModel{ID: m.ID, Name: m.Name, UseCase: m.UseCase, Capabilities: m.Capabilities,
				Task: m.Task, Ready: m.Ready, Gateway: m.Gateway})
		}
		_ = helptext.WriteJSON(os.Stdout, rows)
		return 0
	}
	if len(infos) > 0 && infos[0].Task != "" {
		printEndpointTable(infos)
		if len(gw) > 0 {
			fmt.Println("\nAI GATEWAY (anthropic)")
			printModelTable(gw)
		}
		return 0
	}
	infos = append(infos, gw...)
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
	Usage: "claude-code switch <profile> [flags]",
	About: "Pins the session profile in the vault; relaunch to apply. One launch only:\n" +
		"claude-code --profile <p>.",
	Hint: "claude-code configure provider --list shows each profile's credential state.",
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
		return printPinDiff(map[string]string{providers.PinProfile: args[0]})
	}
	if err := providers.SetPins(map[string]string{providers.PinProfile: args[0]}); err != nil {
		return fail("%v", err)
	}
	fmt.Printf("profile pinned: %s (relaunch to apply)\n", args[0])
	preflight.Warm(os.Stdout, reg, time.Now())
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
	About: "Resolves and prints the session profile's credential: the apiKeyHelper\n" +
		"Claude Code calls. --proxy prints the loopback proxy's client secret instead.",
}

// cmdResolve is the apiKeyHelper: --proxy prints the loopback proxy's client secret instead.
func cmdResolve(args []string) int {
	fs := helptext.NewFlagSet("resolve", resolvePage)
	profile := fs.String("profile", "", "profile name")
	viaRouter := fs.Bool("proxy", false, "print the loopback proxy client secret")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if *viaRouter {
		s, err := proxy.ClientSecret()
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
	sel, err := providers.Select(reg, *profile)
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

// copilotCatalogOptions lists models as the profile copilot runs as (COPILOT_PROFILE), not its stored login.
func copilotCatalogOptions(bin string) copilotsdk.Options {
	return copilotsdk.CatalogOptions(bin, agent.CopilotToken())
}
