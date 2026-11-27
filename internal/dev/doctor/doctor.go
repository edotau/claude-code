package doctor

import (
	"cmp"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/cli/ui/helptext"
	"github.com/edotau/claude-code/internal/cli/ui/table"
	"github.com/edotau/claude-code/internal/dev/preflight"
	"github.com/edotau/claude-code/internal/driver/claude"
	"github.com/edotau/claude-code/internal/platform/audit"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/textutil"
	"github.com/edotau/claude-code/internal/routing/agentconfig/project"
)

// doctorChecks is a test seam over the probing runChecks plus the soft, config-only MCP row.
var doctorChecks = func(ctx context.Context) ([]check, bool) {
	checks, ok := runChecks(ctx)
	mcpOK, detail := summary(mcpOptions(false))
	return append(checks, check{Name: "mcp", OK: mcpOK, Detail: detail}), ok
}

// proxyCheck is a test seam over proxyStatus for `doctor --proxy`.
var proxyCheck = proxyStatus

type doctorOpts struct {
	format                                                                 *string
	auditMode, scan, gitScan, refresh, all, fix, prompts, list, mcp, proxy bool
	days, since                                                            int
	rev, agent                                                             string
}

var doctorPage = helptext.Page{
	Usage: "claude-code doctor [flags] [--scan [path...]]",
	About: "Check binaries, providers, credentials and the proxy; exits 1 when a hard check fails " +
		"(claude binary, provider registry, session credential), soft rows only report. " +
		"Without --fix, writes only caches: the version cache, --refresh's model cache and the credential probe's refreshed token slots. --audit prints the action log (profile " +
		"switches, pins, secret sets, key use, safety blocks, failovers, tailscale and setup actions) " +
		"instead. --scan, --git and --secrets audit for plaintext credentials instead, text only, " +
		"and exit 1 on any finding. --secrets --fix masks idle transcript stores in place; paths restrict this pass. --mcp prints every MCP server's live health, cross-scope drift " +
		"and inline secrets; exits 1 on a failed server, an inline secret or an unreadable config. " +
		"--agent <name> prints that agent's binary, login, route and projection rows and exits 1 when its binary or login fails; its JSON is {ok, checks:[{name, ok, detail}]}, with hard added on hard checks only. --prompts audits the instruction path; with --list it lists every embedded prompt text. The mode flags cannot be combined.",
}

func doctorFlags() (*flag.FlagSet, *doctorOpts) {
	fs := helptext.NewFlagSet("doctor", doctorPage)
	o := &doctorOpts{format: helptext.FormatFlag(fs)}
	fs.BoolVar(&o.auditMode, "audit", false, "print the typed action log instead of health checks")
	fs.IntVar(&o.days, "days", 7, "with --audit/--secrets: last N days (0 = all)")
	fs.BoolVar(&o.scan, "scan", false, "scan paths given, else configs and vault, for inline secrets")
	fs.BoolVar(&o.gitScan, "git", false, "scan this repo's git history for committed secrets")
	fs.IntVar(&o.since, "since", 0, "with --git or --secrets: only the last N commits (0 = all)")
	fs.StringVar(&o.rev, "range", "", "with --git or --secrets: only this revision range, e.g. @{u}..HEAD")
	fs.BoolVar(&o.refresh, "refresh", false, "re-list pinned Databricks models into the cache first")
	fs.BoolVar(&o.all, "secrets", false, "--scan, --git, vault and transcript checks; worst exit wins")
	fs.BoolVar(&o.fix, "fix", false, "with --secrets: mask idle transcript files in place")
	fs.BoolVar(&o.prompts, "prompts", false, "audit the CLAUDE.md, rules, skills and agents instruction path")
	fs.BoolVar(&o.list, "list", false, "with --prompts: list every embedded prompt text")
	fs.BoolVar(&o.mcp, "mcp", false, "MCP server health, cross-scope drift and inline plaintext secrets")
	fs.StringVar(&o.agent, "agent", "", "one agent's binary, login, route and projection rows: "+strings.Join(agent.RosterNames(), "|"))
	fs.BoolVar(&o.proxy, "proxy", false, "run only the proxy check; exit 1 when it fails")
	return fs, o
}

// Run is the `doctor` verb.
func Run(args []string) int {
	fs, o := doctorFlags()
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if len(fs.Args()) > 0 && !o.scan && (!o.all || !o.fix) {
		helptext.Fail("doctor: unexpected arguments %v (paths go after --scan)", fs.Args())
		return 2
	}
	if msg, bad := strayFlags(fs, o); bad {
		return helptext.FailUsage("doctor: %s", msg)
	}
	return runDoctor(o, fs.Args())
}

// strayFlags names the first of --days, --since, --range given without the mode that reads it.
func strayFlags(fs *flag.FlagSet, o *doctorOpts) (string, bool) {
	git := o.gitScan || o.all
	for _, f := range []struct {
		name, mode string
		on         bool
	}{
		{"days", "--audit or --secrets", o.auditMode || o.all}, {"since", "--git or --secrets", git}, {"range", "--git or --secrets", git},
		{"fix", "--secrets", o.all}, {"list", "--prompts", o.prompts},
	} {
		if helptext.Explicit(fs, f.name) && !f.on {
			return fmt.Sprintf("--%s only applies with %s; add it or drop --%s", f.name, f.mode, f.name), true
		}
	}
	return "", false
}

// conflictingModes names the first pair of mode flags that would silently drop one another.
func conflictingModes(o *doctorOpts) (string, bool) {
	var set []string
	for _, m := range []struct {
		on   bool
		name string
	}{
		{o.agent != "", "--agent"}, {o.prompts, "--prompts"}, {o.mcp, "--mcp"}, {o.proxy, "--proxy"}, {o.auditMode, "--audit"}, {o.refresh, "--refresh"},
		{o.scan || o.gitScan || o.all, "--scan/--git/--secrets"},
	} {
		if m.on {
			set = append(set, m.name)
		}
	}
	if len(set) < 2 {
		return "", false
	}
	return set[0] + " cannot combine with " + strings.Join(set[1:], ", "), true
}

func runDoctor(o *doctorOpts, targets []string) int {
	ctx := context.Background()
	if msg, bad := conflictingModes(o); bad {
		helptext.Fail("doctor: %s", msg)
		return 2
	}
	if o.agent != "" {
		return runAgent(ctx, o)
	}
	if o.prompts {
		return runPrompts(o)
	}
	if o.mcp {
		return doctorMCP(os.Stdout, mcpOptions(*o.format == helptext.JSON))
	}
	if o.proxy {
		detail, bad := proxyCheck(ctx)
		clearDegraded(ctx, bad)
		rows := append([]check{{Name: "proxy", OK: !bad, Detail: detail}}, proxyListenerChecks(ctx)...)
		return printChecks(o, rows, !bad)
	}
	if o.scan || o.gitScan || o.all {
		return runDoctorAudits(o, targets)
	}
	if o.auditMode {
		return audit.Report(o.days, *o.format)
	}
	if o.refresh {
		refreshServed(ctx)
	}
	checks, ok := doctorChecks(ctx)
	return printChecks(o, checks, ok)
}

// printChecks renders checks as JSON or text and returns the exit code: 1 when !ok.
func printChecks(o *doctorOpts, checks []check, ok bool) int {
	if *o.format == helptext.JSON {
		if code := helptext.WriteJSONOrFail("doctor", struct {
			OK     bool    `json:"ok"`
			Checks []check `json:"checks"`
		}{ok, checks}); code != 0 {
			return code
		}
	} else {
		printCheckLines(os.Stdout, clipAgentRows(o, checks))
	}
	if !ok {
		return 1
	}
	return 0
}

func runPrompts(o *doctorOpts) int {
	if o.list {
		return listPrompts(o)
	}
	checks := promptPath(paths.ConfigDir())
	ok := true
	for _, c := range checks {
		ok = ok && (c.OK || !c.Hard)
	}
	use := usageReport(filepath.Join(paths.ConfigDir(), "projects"), time.Now(), usageBudget)
	if *o.format == helptext.JSON {
		checks = append(checks, check{Name: "H8 utilization", OK: true, Detail: strings.Join(use, "\n")})
		return printChecks(o, checks, ok)
	}
	code := printChecks(o, checks, ok)
	fmt.Fprintln(os.Stdout, strings.Join(use, "\n"))
	return code
}

// listPrompts prints one row per embedded text: path, family, accessor, template fields, bytes.
func listPrompts(o *doctorOpts) int {
	rows := assets.Inventory()
	if *o.format == helptext.JSON {
		return helptext.WriteJSONOrFail("doctor", rows)
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\n", r.Path, r.Family, strings.Join(r.Accessors, ","), cmp.Or(strings.Join(r.Fields, ","), "-"), r.Size)
	}
	if err := tw.Flush(); err != nil {
		return helptext.Fail("doctor: %v", err)
	}
	return 0
}

// mcpOptions points the MCP scan at the user, project and settings scopes; every path is read-only.
func mcpOptions(asJSON bool) options {
	home, _ := os.UserHomeDir()
	bin, _ := claude.ClaudeBinary() // "" on error: the scan fails OPEN on a missing claude binary
	cwd, _ := os.Getwd()
	return options{
		UserPath:     paths.ClaudeJSON(),
		ProjectFiles: []string{filepath.Join(cwd, ".mcp.json")},
		SettingsFiles: []string{
			filepath.Join(paths.ConfigDir(), "settings.json"),
			filepath.Join(paths.ConfigDir(), "settings.local.json"),
		},
		Claude: bin,
		Home:   home,
		JSON:   asJSON,
	}
}

func runDoctorAudits(o *doctorOpts, targets []string) int {
	if *o.format == helptext.JSON {
		helptext.Fail("doctor: --scan, --git and --secrets print text only")
		return 2
	}
	if o.all && o.days < 0 {
		return helptext.FailUsage("doctor: --days must be nonnegative (0 = all)")
	}
	rc := 0
	if !o.fix || len(targets) == 0 {
		rc = runAudits(os.Stdout, auditOpts{Scan: o.scan || o.all, Git: o.gitScan || o.all, Vault: o.all,
			Targets: targets, Rev: o.rev, Since: o.since})
	}
	if o.all {
		rc = max(rc, transcriptAudit(os.Stdout, o.fix, targets, o.days))
	}
	return rc
}

// refreshServed re-lists every pinned Databricks profile's endpoints into the served-models cache.
func refreshServed(ctx context.Context) {
	for _, err := range preflight.RefreshPinned(ctx) {
		fmt.Fprintf(os.Stderr, "doctor --refresh: %v\n", err)
	}
}

// SpecNode is the `doctor` completion spec.
func SpecNode() *helptext.Node {
	return &helptext.Node{Flags: []helptext.Flag{helptext.BoolFlag("audit"), helptext.Plain("days"), helptext.BoolFlag("scan"), helptext.BoolFlag("git"), helptext.Plain("since"),
		helptext.Plain("range"), helptext.BoolFlag("secrets"), helptext.BoolFlag("fix"), helptext.BoolFlag("refresh"), helptext.BoolFlag("prompts"), helptext.BoolFlag("list"), helptext.BoolFlag("mcp"), helptext.BoolFlag("proxy"), helptext.ValuedFlag("agent", agent.RosterNames), helptext.CompletionFormatFlag()}}
}

// runAgent is `doctor --agent <name>`: that agent's rows; exit 1 when a hard row (binary, login) fails.
func runAgent(ctx context.Context, o *doctorOpts) int {
	s, ok := agent.LookupSchema(o.agent)
	if !ok {
		return helptext.FailUsage("doctor: unknown agent %q (%s)", o.agent, strings.Join(agent.RosterNames(), "|"))
	}
	rows := agentRows(ctx, s)
	pass := true
	for _, r := range rows {
		pass = pass && (r.OK || !r.Hard)
	}
	return printChecks(o, rows, pass)
}

// agentRows are s's binary, login, route and projection rows; claude's are the same four names.
func agentRows(ctx context.Context, s agent.Schema) []check {
	if !s.External() {
		return claudeRows(ctx, s)
	}
	rows := []check{{Name: s.Name + " binary", Hard: true}, {Name: s.Name + " login", Hard: true}}
	p, err := paths.AgentBin(s.Name)
	rows[0].OK, rows[0].Detail = err == nil, orErr(p, err)
	rows[1].OK, rows[1].Detail = s.LoginPresent(), s.Name+" login present"
	if !rows[1].OK {
		rows[1].Detail = "no " + s.Name + " login found (claude-code vault set / agent's own login)"
	}
	return append(rows, routeRow(ctx, s), projectionRow(s))
}

func claudeRows(ctx context.Context, s agent.Schema) []check {
	bin, err := claude.ClaudeBinary()
	rows := []check{{Name: "claude binary", OK: err == nil, Hard: true, Detail: orErr(bin, err)}}
	login := check{Name: "claude login", OK: true, Detail: "not used by claude"}
	if reg, sel, serr, ok := agent.LoadSelection(); ok && serr == nil {
		if cr := providerCredentialChecks(ctx, reg, sel, serr, []string{sel.Provider.Name}); len(cr) == 1 {
			login = check{Name: "claude login", OK: cr[0].OK, Hard: cr[0].Hard, Detail: cr[0].Detail}
		}
	}
	route := check{Name: "claude route", OK: true, Detail: "not used by claude"}
	if _, ok := s.Eng().RouteDetail(ctx); ok {
		route = routeRow(ctx, s)
	}
	return append(rows, login, route, check{Name: "claude projection", OK: true, Detail: "not used by claude"})
}

// routeRow is s's route detail; soft, and "warn" when copilot is off its pinned route.
func routeRow(ctx context.Context, s agent.Schema) check {
	detail, ok := s.Eng().RouteDetail(ctx)
	switch {
	case !ok:
		return check{Name: s.Name + " route", Detail: "warn no route check for " + s.Name}
	case s.RouteWarn(ctx):
		return check{Name: s.Name + " route", Detail: "warn " + detail}
	}
	return check{Name: s.Name + " route", OK: true, Detail: detail}
}

// projectionRow says whether state/projections/<agent>/agents has any rendered agents; soft.
func projectionRow(s agent.Schema) check {
	dir := project.Dir(paths.ConfigDir(), s.Name)
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) == 0 {
		return check{Name: s.Name + " projection", Detail: "warn no rendered agents at " + dir + " (claude-code agents sync)"}
	}
	return check{Name: s.Name + " projection", OK: true, Detail: fmt.Sprintf("%d agent(s) at %s", len(entries), dir)}
}

// clipAgentRows cuts each --agent row's middle to 80 cells, keeping its trailing (claude-code …) repair.
func clipAgentRows(o *doctorOpts, checks []check) []check {
	if o.agent == "" {
		return checks
	}
	out := slices.Clone(checks)
	for i, c := range out {
		budget := 80 - textutil.DisplayWidth("✗ "+c.Name+": ")
		head, tail := c.Detail, ""
		if j := strings.LastIndex(c.Detail, " (claude-code "); j > 0 && strings.HasSuffix(c.Detail, ")") {
			head, tail = c.Detail[:j], c.Detail[j:]
		}
		out[i].Detail = table.TruncateMiddle(head, max(budget-textutil.DisplayWidth(tail), 12)) + tail
	}
	return out
}
