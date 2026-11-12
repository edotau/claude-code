package cli

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/claude"
	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/copilot"
	"github.com/edotau/claude-code/internal/gateway"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/preflight"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/vault"
)

// stdoutf writes formatted text to stdout in one write; a failure is returned, prefixed with prefix when set.
func stdoutf(prefix, format string, a ...any) error {
	if _, err := fmt.Fprintf(os.Stdout, format, a...); err != nil {
		if prefix == "" {
			return err
		}
		return fmt.Errorf("%s: %w", prefix, err)
	}
	return nil
}

// printLines prints each line of a rendered view, returning the first stdout write error.
func printLines(lines []string) error {
	for _, l := range lines {
		if err := stdoutf("", "%s\n", l); err != nil {
			return err
		}
	}
	return nil
}

var modelsPage = helptext.Page{
	Usage: "claude-code models [flags]",
	About: "Shows which model each alias (opus, sonnet, haiku, fable) resolves to on\n" +
		"the session profile. --pin and --unpin write claude's agent record; --live\n" +
		"lists the profile's catalog.",
	Commands: [][2]string{{"discover", "live-list models per profile; --apply saves them"}},
	Hint: "An alias pin on claude's agent record beats the profile's own model.\n" +
		"claude-code models discover -h lists its flags.",
}

// cmdModels is the models verb: alias resolution, pins, the live catalog, and the discover subcommand.
func cmdModels(args []string) int {
	if len(args) > 0 && args[0] == "discover" {
		return modelsDiscover(args[1:])
	}
	fs := helptext.NewFlagSet("models", modelsPage)
	provider := fs.String("profile", "", "profile to resolve (default: pin, then registry default)")
	unpin := fs.Bool("unpin", false, "clear every model pin")
	live := fs.Bool("live", false, "list the models the profile serves (GET /models)")
	dryRun := fs.Bool("dry-run", false, "with --pin/--unpin: print the agent record change, write nothing")
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
	if code := checkModelsArgs(args, *dryRun, *unpin, pins, *format); code != 0 {
		return code
	}
	reg, err := providers.Load()
	if err != nil {
		return helptext.Fail("%v", err)
	}
	if code, handled := configure.ApplyModelPins(reg, *provider, *unpin, pins, modelsPreview(*dryRun)); handled {
		return code
	}
	sel, err := providers.Select(reg, *provider)
	if err != nil {
		return helptext.Fail("%v", err)
	}
	return renderModelSelection(reg, sel, *format, *live)
}

// modelsPreview is the agent-record diff printer for --dry-run, nil for a real write.
func modelsPreview(dryRun bool) func(func(map[string]vault.AgentRecord)) int {
	if !dryRun {
		return nil
	}
	return func(mutate func(map[string]vault.AgentRecord)) int { return printRecordDiff(mutate, nil) }
}

// checkModelsArgs refuses a stray argument, a --dry-run with nothing to preview, or a bad --format (exit 2).
func checkModelsArgs(rest []string, dryRun, unpin bool, pins []string, format string) int {
	if len(rest) > 0 {
		return helptext.FailUsage("models: unexpected argument %q", rest[0])
	}
	if dryRun && !unpin && len(pins) == 0 {
		return helptext.FailUsage("models: --dry-run previews --pin or --unpin; add one or drop --dry-run")
	}
	if err := helptext.CheckFormat(format); err != nil {
		return helptext.BadFormat("models", err)
	}
	return 0
}

func renderModelSelection(reg *providers.Registry, sel providers.Selection, format string, live bool) int {
	if live {
		return listLive(reg, sel.Provider, format)
	}
	if format == helptext.JSON {
		if err := helptext.WriteJSON(os.Stdout, map[string]any{"profile": sel.Provider.Name, "proxy": sel.NeedsRouter(reg), "models": sel.SpecsByAlias()}); err != nil {
			return helptext.Fail("models: %v", err)
		}
		return 0
	}
	if err := printLines(providers.SelectionLines(reg, sel)); err != nil {
		return helptext.Fail("models: %v", err)
	}
	return 0
}

// listLive prints the provider's live model catalog (github-copilot's own SDK, every other provider's /models GET).
func listLive(reg *providers.Registry, p *providers.Provider, format string) int {
	var own func(context.Context) ([]providers.ModelInfo, error)
	if p.Provider == "github" {
		own = copilot.ListModels
	}
	cat, err := providers.LoadCatalog(p, own)
	if err != nil {
		return helptext.Fail("%v", err)
	}
	if cat.Note != "" {
		fmt.Fprintln(os.Stderr, "claude-code: models:", cat.Note)
	}
	if cat.GatewayErr != nil {
		fmt.Fprintln(os.Stderr, "claude-code: models: ai gateway listing unavailable:", cat.GatewayErr)
	}
	if format == helptext.JSON {
		return helptext.WriteJSONOrFail("models", cat.Rows())
	}
	if err := printLines(cat.TableLines()); err != nil {
		return helptext.Fail("models: %v", err)
	}
	return 0
}

var switchPage = helptext.Page{
	Usage: "claude-code switch <profile> [flags]",
	About: "Sets the profile on claude and every other agent that is on and can serve it, and forgets last-used " +
		"profiles; relaunch to apply. One launch only: claude-code --profile <p>.",
	Hint: "claude-code configure provider --list shows each profile's credential state.",
}

// cmdSwitch is the switch verb: it pins the session profile on every agent that can serve it.
func cmdSwitch(args []string) int {
	dryRun, args := helptext.ExtractBool(args, "dry-run")
	if len(args) == 0 || isHelpVerb(args[0]) {
		switchUsage()
		return 0
	}
	if len(args) != 1 {
		switchUsage()
		return 2
	}
	reg, err := providers.Load()
	if err != nil {
		return helptext.Fail("%v", err)
	}
	if _, err := reg.Get(args[0]); err != nil {
		return helptext.Fail("%v", err)
	}
	if dryRun {
		return previewSwitchProfile(reg, args[0])
	}
	if err := configure.SwitchProfile(reg, args[0]); err != nil {
		return helptext.Fail("%v", err)
	}
	fmt.Printf("profile pinned: %s (relaunch to apply)\n", args[0])
	preflight.Warm(os.Stdout, reg, time.Now())
	return 0
}

func previewSwitchProfile(reg *providers.Registry, profile string) int {
	apply := func(m map[string]vault.AgentRecord) { configure.SwitchRecords(reg, m, profile) }
	recs, err := vault.ReadAgentRecords(agent.RosterNames())
	if err != nil {
		return helptext.Fail("switch preview: %v", err)
	}
	return printRecordDiff(apply, keptOff(reg, recs, profile))
}

// switchUsage prints switch's page; helptext.ExtractBool pulls its one flag out, so this FlagSet only renders it.
func switchUsage() {
	fs := helptext.NewFlagSet("switch", switchPage)
	fs.Bool("dry-run", false, "print the agent record change, write nothing")
	fs.Usage()
}

var resolvePage = helptext.Page{
	Usage: "claude-code resolve [flags]",
	About: "Resolves and prints the session profile's credential: the apiKeyHelper\n" +
		"Claude Code calls. --proxy prints a scoped proxy session token, never the client secret:\n" +
		"the launch's HARNESS_PROXY_API_KEY, else one minted for the calling process and cached.",
}

// cmdResolve is the apiKeyHelper: --proxy prints the armed session token, else a scoped token minted for the parent process.
func cmdResolve(args []string) int {
	fs := helptext.NewFlagSet("resolve", resolvePage)
	profile := fs.String("profile", "", "profile name")
	viaRouter := fs.Bool("proxy", false, "print a scoped proxy session token")
	as := fs.String("as", "", "Databricks identity the launch carries: auto|user|sp|headless")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if *viaRouter {
		if tok := os.Getenv(claude.ProxyKeyEnv); tok != "" { // an armed launch's session token, confined to its profile
			fmt.Println(tok)
			return 0
		}
		tok, err := scopedProxyToken(*profile)
		if err != nil {
			return helptext.Fail("resolve --proxy: %v", err) // never falls back to the router secret
		}
		fmt.Println(tok)
		return 0
	}
	reg, err := providers.Load()
	if err != nil {
		return helptext.Fail("%v", err)
	}
	sel, err := providers.Select(reg, *profile)
	if err != nil {
		return helptext.Fail("%v", err)
	}
	tok, err := providers.Credential(providers.WithIdentity(context.Background(), *as), sel.Provider)
	if err != nil {
		return helptext.Fail("%v", err)
	}
	fmt.Fprintln(os.Stdout, tok)
	return 0
}

// resolveProxyToken is the mint+cache seam of resolve --proxy.
var resolveProxyToken = gateway.ResolveProxyToken

// scopedProxyToken mints (or reuses) the session token the launcher would arm for the calling claude process.
func scopedProxyToken(profile string) (string, error) {
	reg, err := providers.Load()
	if err != nil {
		return "", err
	}
	sel, err := providers.Select(reg, profile)
	if err != nil {
		return "", err
	}
	var aliases []string
	for _, t := range sel.Aliases {
		aliases = append(aliases, t.Provider.Name)
	}
	scope, err := agent.TokenScope(sel.Provider.Name, aliases...)
	if err != nil {
		return "", err
	}
	return resolveProxyToken(context.Background(), scope, os.Getppid())
}

// keptOff lists "agent: off (kept)" for each off agent that could serve profile and a switch leaves off.
func keptOff(reg *providers.Registry, recs map[string]vault.AgentRecord, profile string) []string {
	_, off := configure.SwitchTargets(reg, recs, profile)
	lines := make([]string, len(off))
	for i, n := range off {
		lines[i] = n + ": off (kept)"
	}
	return lines
}

// printRecordDiff is the shared --dry-run body for switch and models.
func printRecordDiff(apply func(map[string]vault.AgentRecord), notes []string) int {
	before, err := vault.ReadAgentRecords(agent.RosterNames())
	if err != nil {
		return helptext.Fail("agent record preview: %v", err)
	}
	after := maps.Clone(before)
	apply(after)
	if err := printLines(providers.DryRunLines(before, after, notes)); err != nil {
		return helptext.Fail("agent record preview: %v", err)
	}
	return 0
}

var modelsDiscoverPage = helptext.Page{
	Usage: "claude-code models discover [flags]",
	About: "Live-discover models for one profile, or every registry profile (sorted).\nPreview by default; --apply appends sealed, profile-scoped selectable entries.\nNever removes models, changes pins/routes, or treats listing as inference proof.",
	Hint:  "Vendor-owned catalogs are not rewritten. /discover-models defaults to --apply.",
}

func modelsDiscover(args []string) int {
	fs := helptext.NewFlagSet("models discover", modelsDiscoverPage)
	profile := fs.String("profile", "", "discover only this registry profile")
	apply := fs.Bool("apply", false, "append discovered additions to saved runtime pickers")
	dry := fs.Bool("dry-run", false, "preview only (the default); conflicts with --apply")
	format := helptext.FormatFlag(fs)
	rest, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(rest) > 0 {
		return helptext.FailUsage("models discover: unexpected argument %q", rest[0])
	}
	if *apply && *dry {
		return helptext.FailUsage("models discover: --apply and --dry-run conflict; drop one")
	}
	if err := helptext.CheckFormat(*format); err != nil {
		return helptext.BadFormat("models discover", err)
	}
	reg, err := providers.Load()
	if err != nil {
		return helptext.Fail("models discover: cannot load provider registry: %v", err)
	}
	names, code := discoverNames(reg, *profile)
	if code != 0 {
		return code
	}
	results, ok := copilot.DiscoverPickerProfiles(reg, names, *apply)
	if !ok {
		code = 1
	}
	return printDiscoverResults(results, *apply, *format, code)
}

// discoverNames is the sorted registry profile names, or just profile; an unknown one exits 2.
func discoverNames(reg *providers.Registry, profile string) ([]string, int) {
	if profile == "" {
		names := reg.Names()
		slices.Sort(names)
		return names, 0
	}
	if _, err := reg.Get(profile); err != nil {
		return nil, helptext.FailUsage("models discover: unknown profile %q", profile)
	}
	return []string{profile}, 0
}

func printDiscoverResults(results []copilot.DiscoverResult, apply bool, format string, code int) int {
	if format == helptext.JSON {
		if err := helptext.WriteJSON(os.Stdout, struct {
			Apply   bool                     `json:"apply"`
			Results []copilot.DiscoverResult `json:"results"`
		}{apply, results}); err != nil {
			return helptext.Fail("models discover: cannot write JSON: %v", err)
		}
		return code
	}
	if err := printDiscoverText(results); err != nil {
		return helptext.Fail("models discover: cannot write text output: %v", err)
	}
	return code
}

// printDiscoverText renders the results into one buffer and writes it once, so a stdout write error is returned.
func printDiscoverText(results []copilot.DiscoverResult) error {
	var b strings.Builder
	for _, r := range results {
		fmt.Fprintf(&b, "%s/%s: %s source=%s discovered=%d additions=%d\n", r.Provider, r.Profile, r.Status, cmp.Or(r.Source, "none"), r.Discovered, len(r.Additions))
		for _, m := range r.Additions {
			fmt.Fprintf(&b, "  + %s\n", m.ID)
		}
		if len(r.Destinations) > 0 {
			fmt.Fprintf(&b, "  destinations: %s\n", strings.Join(r.Destinations, ", "))
		}
		if r.Note != "" {
			fmt.Fprintf(&b, "  %s\n", r.Note)
		}
	}
	b.WriteString("Saved entries are append-only choices, not current availability or inference proof. Native vendor catalogs are unchanged.\n")
	_, err := io.WriteString(os.Stdout, b.String())
	return err
}

// providerList prints every provider's credential state, or one provider's detail.
func providerList(name, format string) int {
	if err := helptext.CheckFormat(format); err != nil {
		return helptext.BadFormat("configure provider", err)
	}
	reg, err := providers.Load()
	if err != nil {
		return helptext.Fail("%v", err)
	}
	if format == helptext.JSON {
		return providerListJSON(reg, name)
	}
	if name == "" {
		return printProviderTable(reg)
	}
	p, err := reg.Get(name)
	if err != nil {
		return helptext.Fail("%v", err)
	}
	source, note := providers.CredSource(p)
	if err := printLines(providers.ProfileDetail(p, source, note)); err != nil {
		return helptext.Fail("configure provider: %v", err)
	}
	return 0
}

// printProviderTable prints the provider and agent-CLI summary of the profiles whose credential resolves now.
func printProviderTable(reg *providers.Registry) int {
	var agents []providers.AgentServer
	for _, v := range agent.Names() {
		agents = append(agents, providers.AgentServer{Name: v, Serves: func(p *providers.Provider) bool { return agent.CanServe(v, p) }})
	}
	ready := func(p *providers.Provider) bool { src, _ := providers.CredSource(p); return src != "missing" }
	if err := printLines(providers.ListTable(reg, ready, agents)); err != nil {
		return helptext.Fail("configure provider: %v", err)
	}
	return 0
}

// providerEntry is one provider in the JSON listing: its shape and where its credential resolves from, never the value.
type providerEntry struct {
	Name        string            `json:"name"`
	Protocol    string            `json:"protocol"`
	Dialects    []string          `json:"dialects"`
	Auth        string            `json:"auth"`
	Login       string            `json:"login,omitempty"`
	Credential  bool              `json:"credential"`
	Models      map[string]string `json:"models,omitempty"`
	Group       string            `json:"group"` // the family `--list` files it under (anthropic, openai, ... local)
	Source      string            `json:"source"`
	Note        string            `json:"note"`
	Host        string            `json:"host,omitempty"` // non-secret: preset host, else BaseURL's scheme+host
	AccountID   string            `json:"accountId,omitempty"`
	WorkspaceID string            `json:"workspaceId,omitempty"`
}

// newProviderEntry builds p's listing entry; p.Name is its registry key.
func newProviderEntry(p *providers.Provider) providerEntry {
	e := providerEntry{
		Name: p.Name, Protocol: p.Protocol, Dialects: p.Dialects(), Auth: p.Auth.Method(), Login: p.Auth.Login,
		Credential: providers.CredentialPresent(p), Models: p.Models, Group: p.Provider,
		Host: providers.AuthHost(p), AccountID: p.Auth.AccountID, WorkspaceID: p.Auth.WorkspaceID,
	}
	e.Source, e.Note = providers.CredSource(p)
	return e
}

// providerListJSON emits the provider listing: all providers, or the one named, never a credential.
func providerListJSON(reg *providers.Registry, name string) int {
	if name != "" {
		p, err := reg.Get(name)
		if err != nil {
			return helptext.Fail("%v", err)
		}
		return helptext.WriteJSONOrFail("configure provider", newProviderEntry(p))
	}
	names := reg.Names()
	list := make([]providerEntry, 0, len(names))
	for _, n := range names {
		list = append(list, newProviderEntry(reg.Providers[n]))
	}
	var session string // the profile a launch resolves to today
	if sel, err := providers.Select(reg, ""); err == nil && sel.Provider != nil {
		session = sel.Provider.Name
	}
	return helptext.WriteJSONOrFail("configure provider", struct {
		Session  string          `json:"session"`
		Default  string          `json:"default"`
		Fallback []string        `json:"fallback,omitempty"`
		List     []providerEntry `json:"list"`
	}{session, reg.Default, reg.Fallback, list})
}

// modelPinAliases offers "alias=" stems for --pin; the user types the model id after the "=" themselves.
func modelPinAliases() []string {
	out := make([]string, 0, len(models.AliasNames)+1)
	for _, s := range models.AliasNames {
		out = append(out, s+"=")
	}
	return append(out, "all=")
}

// modelsSpec is the completion node for the models verb.
func modelsSpec() *helptext.Node {
	discover := &helptext.Node{Flags: []helptext.Flag{helptext.ValuedFlag("profile", providers.ProfileNames), helptext.BoolFlag("apply"),
		helptext.BoolFlag("dry-run"), helptext.CompletionFormatFlag()}}
	return &helptext.Node{Sub: map[string]*helptext.Node{"discover": discover}, Flags: []helptext.Flag{helptext.ValuedFlag("profile", providers.ProfileNames),
		helptext.BoolFlag("unpin"), helptext.BoolFlag("live"), {Name: "pin", Values: modelPinAliases}, helptext.BoolFlag("dry-run"), helptext.CompletionFormatFlag()}}
}

// switchSpec is the completion node for the switch verb.
func switchSpec() *helptext.Node {
	return &helptext.Node{Args: []func() []string{providers.ProfileNames}, Flags: []helptext.Flag{helptext.BoolFlag("dry-run")}}
}

// resolveSpec is the completion node for the resolve verb.
func resolveSpec() *helptext.Node {
	identities := func() []string { return []string{"auto", "user", "sp", "headless"} }
	return &helptext.Node{Flags: []helptext.Flag{helptext.ValuedFlag("profile", providers.ProfileNames), helptext.BoolFlag("proxy"),
		helptext.ValuedFlag("as", identities)}}
}
