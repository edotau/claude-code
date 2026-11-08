package doctor

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/completion"
	"github.com/edotau/claude-code/internal/launch"
	"github.com/edotau/claude-code/internal/preflight"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// doctorChecks is a test seam over the slow, probing launch.DoctorChecks.
var doctorChecks = launch.DoctorChecks

type doctorOpts struct {
	format                                 *string
	auditMode, scan, gitScan, refresh, all bool
	days, since                            int
	rev                                    string
}

var doctorPage = helptext.Page{
	Usage: "claude-code doctor [flags] [--scan [path...]]",
	About: "Check binaries, providers, credentials and the proxy without changing anything; " +
		"exits 1 when a check fails. --audit prints switches, pins, secret sets, safety blocks " +
		"and proxy failovers instead. --scan, --git and --secrets audit for plaintext " +
		"credentials instead and exit 1 on any finding.",
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

func runDoctor(o *doctorOpts, targets []string) int {
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
	if *o.format == helptext.JSON {
		if err := helptext.WriteJSON(os.Stdout, struct {
			OK     bool           `json:"ok"`
			Checks []launch.Check `json:"checks"`
		}{ok, checks}); err != nil {
			return helptext.Fail("doctor: %v", err)
		}
	} else {
		launch.PrintChecks(os.Stdout, checks)
	}
	if !ok {
		return 1
	}
	return 0
}

func runDoctorAudits(o *doctorOpts, targets []string) int {
	if *o.format == helptext.JSON {
		helptext.Fail("doctor: --scan, --git and --secrets print text only")
		return 2
	}
	return providers.RunAudits(os.Stdout, providers.AuditOpts{Scan: o.scan || o.all, Git: o.gitScan || o.all, Vault: o.all,
		Targets: targets, Rev: o.rev, Since: o.since, VaultDoctor: launch.VaultDoctor})
}

// refreshServed re-lists every pinned Databricks profile's endpoints into the served-models cache.
func refreshServed() {
	for _, err := range preflight.RefreshPinned(context.Background()) {
		fmt.Fprintf(os.Stderr, "doctor --refresh: %v\n", err)
	}
}

// SpecNode is the `doctor` completion spec.
func SpecNode() *completion.Node {
	return &completion.Node{Flags: []completion.Flag{completion.BoolFlag("audit"), completion.Plain("days"), completion.BoolFlag("scan"), completion.BoolFlag("git"), completion.Plain("since"),
		completion.Plain("range"), completion.BoolFlag("secrets"), completion.BoolFlag("refresh"), completion.FormatFlag()}}
}
