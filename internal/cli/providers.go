package cli

import (
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/agentcmd"
	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/copilot"
	"github.com/edotau/claude-code/internal/databricks"
	"github.com/edotau/claude-code/internal/describe"
	"github.com/edotau/claude-code/internal/gateway"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/preflight"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/ui/tui"
	"github.com/edotau/claude-code/internal/vault"
)

// printLines prints each line of a rendered view, returning the first stdout write error.
func printLines(lines []string) error {
	for _, l := range lines {
		if _, err := fmt.Fprintln(os.Stdout, l); err != nil {
			return err
		}
	}
	return nil
}

var modelsPage = helptext.Page{
	Usage: "claude-code models [flags]",
	About: "Shows which model each alias (opus, sonnet, haiku, fable) resolves to on\n" +
		"the session profile. --pin and --unpin write the vault pins; --live lists the\n" +
		"profile's catalog.",
	Hint: "A CLAUDE_<ALIAS>_MODEL vault pin beats the profile's own model.",
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
	if len(args) > 0 {
		fmt.Fprintf(os.Stderr, "claude-code: models: unexpected argument %q\n", args[0])
		return 2
	}
	if err := helptext.CheckFormat(*format); err != nil {
		return helptext.BadFormat("models", err)
	}
	reg, err := providers.Load()
	if err != nil {
		return helptext.Fail("%v", err)
	}
	var preview func(func(map[string]vault.AgentRecord)) int
	if *dryRun {
		preview = func(mutate func(map[string]vault.AgentRecord)) int { return printRecordDiff(mutate, nil) }
	}
	if code, handled := configure.ApplyModelPins(reg, *provider, *unpin, pins, preview); handled {
		return code
	}
	sel, err := providers.Select(reg, *provider)
	if err != nil {
		return helptext.Fail("%v", err)
	}
	return renderModelSelection(reg, sel, *format, *live)
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
	cat, err := providers.LoadCatalog(reg, p, own)
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
	About: "Sets the profile on every agent that can serve it; relaunch to apply. One launch only:\n" +
		"claude-code --profile <p>.",
	Hint: "claude-code configure provider --list shows each profile's credential state.",
}

// cmdSwitch is the switch verb: it pins the session profile on every agent that can serve it.
func cmdSwitch(args []string) int {
	dryRun, args := helptext.ExtractBool(args, "dry-run")
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
		"Claude Code calls. --proxy prints the loopback proxy's client secret instead.",
}

// cmdResolve is the apiKeyHelper: --proxy prints the loopback proxy's client secret instead.
func cmdResolve(args []string) int {
	fs := helptext.NewFlagSet("resolve", resolvePage)
	profile := fs.String("profile", "", "profile name")
	viaRouter := fs.Bool("proxy", false, "print the loopback proxy client secret")
	as := fs.String("as", "", "Databricks identity the launch carries: auto|user|sp|headless")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if *viaRouter {
		s, err := gateway.ClientSecret()
		if err != nil {
			return helptext.Fail("%v", err)
		}
		fmt.Println(s)
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
	Hint:  "Vendor-owned native catalogs are not rewritten. /discover-models defaults to --apply.",
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
	if len(rest) > 0 || (*apply && *dry) {
		fmt.Fprintln(os.Stderr, "claude-code: models discover: unexpected argument or conflicting --apply/--dry-run")
		return 2
	}
	if err := helptext.CheckFormat(*format); err != nil {
		return helptext.BadFormat("models discover", err)
	}
	reg, err := providers.Load()
	if err != nil {
		return helptext.Fail("models discover: cannot load provider registry")
	}
	names := reg.Names()
	slices.Sort(names)
	if *profile != "" {
		if _, err := reg.Get(*profile); err != nil {
			fmt.Fprintln(os.Stderr, "claude-code: models discover: unknown profile")
			return 2
		}
		names = []string{*profile}
	}
	results, ok := copilot.DiscoverPickerProfiles(reg, names, *apply)
	code := 0
	if !ok {
		code = 1
	}
	return printDiscoverResults(results, *apply, *format, code)
}

func printDiscoverResults(results []copilot.DiscoverResult, apply bool, format string, code int) int {
	if format == helptext.JSON {
		if err := helptext.WriteJSON(os.Stdout, struct {
			Apply   bool                     `json:"apply"`
			Results []copilot.DiscoverResult `json:"results"`
		}{apply, results}); err != nil {
			return helptext.Fail("models discover: cannot write JSON")
		}
		return code
	}
	if err := printDiscoverText(results); err != nil {
		return helptext.Fail("models discover: cannot write text output")
	}
	return code
}

// printDiscoverText renders the results into one buffer and writes it once, so a stdout write error is returned.
func printDiscoverText(results []copilot.DiscoverResult) error {
	var b strings.Builder
	for _, r := range results {
		fmt.Fprintf(&b, "%s/%s: %s source=%s discovered=%d additions=%d\n", r.Provider, r.Profile, r.Status, r.Source, r.Discovered, len(r.Additions))
		for _, m := range r.Additions {
			fmt.Fprintf(&b, "  + %s\n", m.ID)
		}
		if len(r.Destinations) > 0 {
			fmt.Fprintf(&b, "  destinations: %v\n", r.Destinations)
		}
		if r.Note != "" {
			fmt.Fprintf(&b, "  %s\n", r.Note)
		}
	}
	b.WriteString("Saved entries are append-only choices, not current availability or inference proof. Native vendor catalogs are unchanged.\n")
	_, err := io.WriteString(os.Stdout, b.String())
	return err
}

// providerList prints every provider's credential state, or one provider's detail; json mirrors describe.
func providerList(name, format string, verbs func() []describe.Verb) int {
	if err := helptext.CheckFormat(format); err != nil {
		return helptext.BadFormat("configure provider", err)
	}
	reg, err := providers.Load()
	if err != nil {
		return helptext.Fail("%v", err)
	}
	if format == helptext.JSON {
		return providerListJSON(reg, name, verbs())
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

// providerListJSON emits the describe provider view: all providers, or the one named, never a credential.
func providerListJSON(reg *providers.Registry, name string, verbs []describe.Verb) int {
	d, err := describe.Build(reg, verbs)
	if err != nil {
		return helptext.Fail("configure provider: %v", err)
	}
	var want *providers.Provider
	if name != "" {
		if want, err = reg.Get(name); err != nil {
			return helptext.Fail("%v", err)
		}
	}
	list := d.ProviderEntries(reg, want)
	if want != nil {
		if len(list) == 0 {
			return helptext.Fail("configure provider: %q not in the provider list", name)
		}
		return helptext.WriteJSONOrFail("configure provider", list[0])
	}
	return helptext.WriteJSONOrFail("configure provider", struct {
		Session  string                       `json:"session"`
		Default  string                       `json:"default"`
		Fallback []string                     `json:"fallback,omitempty"`
		List     []describe.ProviderListEntry `json:"list"`
	}{d.Providers.Session, d.Providers.Default, d.Providers.Fallback, list})
}

func profileFlag() helptext.Flag {
	return helptext.Flag{Name: "profile", Values: providers.ProfileNames}
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
	return &helptext.Node{Flags: []helptext.Flag{profileFlag(), helptext.BoolFlag("unpin"), helptext.BoolFlag("live"),
		{Name: "pin", Values: modelPinAliases}, helptext.BoolFlag("dry-run"), helptext.CompletionFormatFlag()}}
}

// switchSpec is the completion node for the switch verb.
func switchSpec() *helptext.Node {
	return &helptext.Node{Args: []func() []string{providers.ProfileNames}, Flags: []helptext.Flag{helptext.BoolFlag("dry-run")}}
}

// resolveSpec is the completion node for the resolve verb.
func resolveSpec() *helptext.Node {
	return &helptext.Node{Flags: []helptext.Flag{profileFlag(), helptext.BoolFlag("proxy")}}
}

func init() {
	databricks.Pick = tui.Pick
	databricks.Interactive = tui.StdinIsTerminal
	agentcmd.ModelsVerb = cmdModels // agentcmd cannot import cli: `<agent> catalog` lists through this verb
}
