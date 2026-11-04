package cli

import (
	"context"
	"fmt"
	"maps"
	"os"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/copilot"
	"github.com/edotau/claude-code/internal/preflight"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/vault"
)

// printLines prints each line of a rendered view.
func printLines(lines []string) {
	for _, l := range lines {
		fmt.Println(l)
	}
}

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
	source, note := providers.CredSource(p)
	printLines(providers.ProfileDetail(p, source, note))
	return 0
}

// printProviderTable prints the provider and agent-CLI summary of the profiles whose credential resolves now.
func printProviderTable(reg *providers.Registry) {
	var agents []providers.AgentServer
	for _, v := range agent.Names() {
		agents = append(agents, providers.AgentServer{Name: v, Serves: func(p *providers.Provider) bool { return agent.CanServe(v, p) }})
	}
	ready := func(p *providers.Provider) bool { src, _ := providers.CredSource(p); return src != "missing" }
	printLines(providers.ListTable(reg, ready, agents))
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
	var want *providers.Provider
	if name != "" {
		if want, err = reg.Get(name); err != nil {
			return fail("%v", err)
		}
	}
	list := make([]providerListEntry, 0, len(d.Providers.List))
	for _, dp := range d.Providers.List {
		p := reg.Providers[dp.Name]
		if want != nil && p != want {
			continue
		}
		e := providerListEntry{DescribeProvider: dp}
		e.Group = p.Provider
		e.Source, e.Note = providers.CredSource(p)
		e.Host, e.AccountID, e.WorkspaceID = providers.AuthHost(p), p.Auth.AccountID, p.Auth.WorkspaceID
		list = append(list, e)
	}
	if want != nil {
		if len(list) == 0 {
			return fail("configure provider: %q not in the provider list", name)
		}
		return writeJSONOrFail("configure provider", list[0])
	}
	return writeJSONOrFail("configure provider", struct {
		Session  string              `json:"session"`
		Default  string              `json:"default"`
		Fallback []string            `json:"fallback,omitempty"`
		List     []providerListEntry `json:"list"`
	}{d.Providers.Session, d.Providers.Default, d.Providers.Fallback, list})
}

var modelsPage = helptext.Page{
	Usage: "claude-code models [flags]",
	About: "Shows which model each alias (opus, sonnet, haiku, fable) resolves to on\n" +
		"the session profile. --pin and --unpin write the vault pins; --live lists the\n" +
		"profile's catalog.",
	Hint: "A CLAUDE_<ALIAS>_MODEL vault pin beats the profile's own model.",
}

func cmdModels(args []string) int {
	if len(args) > 0 && args[0] == "discover" {
		return cmdModelsDiscover(args[1:])
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
	set, err := agentconfig.ParseModelPins(reg, provider, unpin, pins)
	if err != nil {
		return fail("%v", err), true
	}
	warn, err := copilot.ValidateCopilotPins(provider, pins)
	if err != nil {
		return fail("%v", err), true
	}
	if warn != "" {
		fmt.Fprintln(os.Stderr, "claude-code: models:", warn)
	}
	if dryRun {
		return printRecordDiff(func(m map[string]vault.AgentRecord) { m["claude"] = agentconfig.WithClaudeModels(m["claude"], set) }, nil), true
	}
	if err := agentconfig.ApplyClaudeModels(set); err != nil {
		return fail("%v", err), true
	}
	return 0, false
}

func renderModelSelection(reg *providers.Registry, sel providers.Selection, format string, live bool) int {
	if live {
		return listLive(reg, sel.Provider, format)
	}
	if format == helptext.JSON {
		if err := helptext.WriteJSON(os.Stdout, map[string]any{"profile": sel.Provider.Name, "proxy": sel.NeedsRouter(reg), "models": sel.SpecsByAlias()}); err != nil {
			return fail("models: %v", err)
		}
		return 0
	}
	printLines(providers.SelectionLines(reg, sel))
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
		return fail("%v", err)
	}
	if cat.Note != "" {
		fmt.Fprintln(os.Stderr, "claude-code: models:", cat.Note)
	}
	if cat.GatewayErr != nil {
		fmt.Fprintln(os.Stderr, "claude-code: models: ai gateway listing unavailable:", cat.GatewayErr)
	}
	if format == helptext.JSON {
		return writeJSONOrFail("models", cat.Rows())
	}
	printLines(cat.TableLines())
	return 0
}

var switchPage = helptext.Page{
	Usage: "claude-code switch <profile> [flags]",
	About: "Sets the profile on every agent that can serve it; relaunch to apply. One launch only:\n" +
		"claude-code --profile <p>.",
	Hint: "claude-code configure provider --list shows each profile's credential state.",
}

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
		return fail("%v", err)
	}
	if _, err := reg.Get(args[0]); err != nil {
		return fail("%v", err)
	}
	if dryRun {
		apply := func(m map[string]vault.AgentRecord) { configure.SwitchRecords(reg, m, args[0]) }
		recs, err := vault.ReadAgentRecords(agent.RosterNames())
		if err != nil {
			return fail("switch preview: %v", err)
		}
		return printRecordDiff(apply, keptOff(reg, recs, args[0]))
	}
	if err := configure.SwitchProfile(reg, args[0]); err != nil {
		return fail("%v", err)
	}
	fmt.Printf("profile pinned: %s (relaunch to apply)\n", args[0])
	preflight.Warm(os.Stdout, reg, time.Now())
	return 0
}

// switchUsage prints switch's page; its one flag is pulled out by helptext.ExtractBool, so this FlagSet only renders it.
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
		return fail("agent record preview: %v", err)
	}
	after := maps.Clone(before)
	apply(after)
	printLines(providers.DryRunLines(before, after, notes))
	return 0
}
