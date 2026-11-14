package doctor

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/claude"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/preflight"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// doctorChecks is a test seam over the probing runChecks plus the soft, config-only MCP row.
var doctorChecks = func(ctx context.Context) ([]check, bool) {
	checks, ok := runChecks(ctx)
	mcpOK, detail := summary(mcpOptions(false))
	return append(checks, check{Name: "mcp", OK: mcpOK, Detail: detail}), ok
}

type doctorOpts struct {
	format                                                            *string
	auditMode, scan, gitScan, refresh, all, prompts, mcp, permissions bool
	days, since                                                       int
	rev, agent                                                        string
}

var doctorPage = helptext.Page{
	Usage: "claude-code doctor [flags] [--scan [path...]]",
	About: "Check binaries, providers, credentials and the proxy; exits 1 when a hard check fails " +
		"(claude binary, provider registry, session credential), soft rows only report. " +
		"Changes nothing except the version cache and --refresh's model cache. --audit prints the action log (profile " +
		"switches, pins, secret sets, key use, safety blocks, failovers, tailscale and setup actions) " +
		"instead. --scan, --git and --secrets audit for plaintext credentials instead, text only, " +
		"and exit 1 on any finding. --mcp prints every MCP server's live health, cross-scope drift " +
		"and inline secrets; exits 1 on a failed server, an inline secret or an unreadable config. " +
		"--prompts audits the instruction path. --permissions checks every agent's launch policy against the jobs " +
		"its definition needs, with no model call; exits 1 on a missed expectation. The mode flags cannot be combined.",
}

func doctorFlags() (*flag.FlagSet, *doctorOpts) {
	fs := helptext.NewFlagSet("doctor", doctorPage)
	o := &doctorOpts{format: helptext.FormatFlag(fs)}
	fs.BoolVar(&o.auditMode, "audit", false, "print the typed action log instead of health checks")
	fs.IntVar(&o.days, "days", 7, "with --audit: events from the last N days")
	fs.BoolVar(&o.scan, "scan", false, "scan paths given, else configs and vault, for inline secrets")
	fs.BoolVar(&o.gitScan, "git", false, "scan this repo's git history for committed secrets")
	fs.IntVar(&o.since, "since", 0, "with --git or --secrets: only the last N commits (0 = all)")
	fs.StringVar(&o.rev, "range", "", "with --git or --secrets: only this revision range, e.g. @{u}..HEAD")
	fs.BoolVar(&o.refresh, "refresh", false, "re-list pinned Databricks models into the cache first")
	fs.BoolVar(&o.all, "secrets", false, "--scan, --git and the vault checks together; worst exit wins")
	fs.BoolVar(&o.prompts, "prompts", false, "audit the CLAUDE.md, rules, skills and agents instruction path")
	fs.BoolVar(&o.mcp, "mcp", false, "MCP server health, cross-scope drift and inline plaintext secrets")
	fs.BoolVar(&o.permissions, "permissions", false, "check each agent's launch policy against its jobs")
	fs.StringVar(&o.agent, "agent", "", "with --permissions: only this agent")
	return fs, o
}

// Run is the `doctor` verb.
func Run(args []string) int {
	fs, o := doctorFlags()
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if err := helptext.CheckFormat(*o.format); err != nil {
		return helptext.BadFormat("doctor", err)
	}
	if len(fs.Args()) > 0 && !o.scan {
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
		{"days", "--audit", o.auditMode}, {"since", "--git or --secrets", git}, {"range", "--git or --secrets", git},
		{"agent", "--permissions", o.permissions},
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
		{o.prompts, "--prompts"}, {o.mcp, "--mcp"}, {o.permissions, "--permissions"}, {o.auditMode, "--audit"}, {o.refresh, "--refresh"},
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
	if o.prompts {
		return runPrompts(o)
	}
	if o.permissions {
		return doctorPermissions(os.Stdout, o.agent, *o.format == helptext.JSON)
	}
	if o.mcp {
		return doctorMCP(os.Stdout, mcpOptions(*o.format == helptext.JSON))
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
		if err := helptext.WriteJSON(os.Stdout, struct {
			OK     bool    `json:"ok"`
			Checks []check `json:"checks"`
		}{ok, checks}); err != nil {
			return helptext.Fail("doctor: %v", err)
		}
	} else {
		printCheckLines(os.Stdout, checks)
	}
	if !ok {
		return 1
	}
	return 0
}

func runPrompts(o *doctorOpts) int {
	checks := promptPath(paths.ConfigDir())
	ok := true
	for _, c := range checks {
		ok = ok && c.OK
	}
	return printChecks(o, checks, ok)
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
	return runAudits(os.Stdout, auditOpts{Scan: o.scan || o.all, Git: o.gitScan || o.all, Vault: o.all,
		Targets: targets, Rev: o.rev, Since: o.since})
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
		helptext.Plain("range"), helptext.BoolFlag("secrets"), helptext.BoolFlag("refresh"), helptext.BoolFlag("prompts"), helptext.BoolFlag("mcp"), helptext.BoolFlag("permissions"), helptext.Plain("agent"), helptext.CompletionFormatFlag()}}
}
