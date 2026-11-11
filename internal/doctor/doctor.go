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

// doctorChecks is a test seam over the probing launch.DoctorChecks plus the soft, config-only MCP row.
var doctorChecks = func() ([]check, bool) {
	checks, ok := runChecks()
	mcpOK, detail := summary(mcpOptions(false))
	return append(checks, check{Name: "mcp", OK: mcpOK, Detail: detail}), ok
}

type doctorOpts struct {
	format                                               *string
	auditMode, scan, gitScan, refresh, all, prompts, mcp bool
	days, since                                          int
	rev                                                  string
}

var doctorPage = helptext.Page{
	Usage: "claude-code doctor [flags] [--scan [path...]]",
	About: "Check binaries, providers, credentials and the proxy without changing anything; " +
		"exits 1 when a check fails. --audit prints switches, pins, secret sets, safety blocks " +
		"and proxy failovers instead. --scan, --git and --secrets audit for plaintext " +
		"credentials instead and exit 1 on any finding. --mcp prints every MCP server's live " +
		"health, cross-scope drift and inline secrets; exit 1 on any.",
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
	fs.BoolVar(&o.refresh, "refresh", false, "re-list each pinned Databricks workspace's served models before the checks")
	fs.BoolVar(&o.all, "secrets", false, "--scan, --git and the vault checks together; worst exit wins")
	fs.BoolVar(&o.prompts, "prompts", false, "audit the CLAUDE.md, rules, skills and agents instruction path")
	fs.BoolVar(&o.mcp, "mcp", false, "MCP server health, cross-scope drift and inline plaintext secrets")
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
	return runDoctor(o, fs.Args())
}

// conflictingModes names the first pair of mode flags that would silently drop one another.
func conflictingModes(o *doctorOpts) (string, bool) {
	var set []string
	for _, m := range []struct {
		on   bool
		name string
	}{
		{o.prompts, "--prompts"}, {o.mcp, "--mcp"}, {o.auditMode, "--audit"}, {o.refresh, "--refresh"},
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
	if msg, bad := conflictingModes(o); bad {
		helptext.Fail("doctor: %s", msg)
		return 2
	}
	if o.prompts {
		return runPrompts(o)
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
		refreshServed()
	}
	checks, ok := doctorChecks()
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
func refreshServed() {
	for _, err := range preflight.RefreshPinned(context.Background()) {
		fmt.Fprintf(os.Stderr, "doctor --refresh: %v\n", err)
	}
}

// SpecNode is the `doctor` completion spec.
func SpecNode() *helptext.Node {
	return &helptext.Node{Flags: []helptext.Flag{helptext.BoolFlag("audit"), helptext.Plain("days"), helptext.BoolFlag("scan"), helptext.BoolFlag("git"), helptext.Plain("since"),
		helptext.Plain("range"), helptext.BoolFlag("secrets"), helptext.BoolFlag("refresh"), helptext.BoolFlag("prompts"), helptext.BoolFlag("mcp"), helptext.CompletionFormatFlag()}}
}
