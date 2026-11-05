package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/copilot"
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

func doctorFlags() (*flag.FlagSet, *doctorOpts) {
	fs := helptext.NewFlagSet("doctor", helptext.Page{
		Usage: "claude-code doctor [flags] [--scan [path...]]",
		About: "Check binaries, providers, credentials and the proxy without changing anything; " +
			"exits 1 when a check fails. --audit prints switches, pins, secret sets, safety blocks " +
			"and proxy failovers instead. --scan, --git and --secrets audit for plaintext " +
			"credentials instead and exit 1 on any finding.",
	})
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

func cmdDoctor(args []string) int {
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
		return printAudit(o.days, *o.format)
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

// preflightRefresh re-lists stale served-endpoint caches within the preflight budget, silently.
func preflightRefresh(reg *providers.Registry) {
	now := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), preflight.Budget)
	defer cancel()
	preflight.RefreshStale(ctx, reg, preflight.Check(reg, now), now, false)
}

// preflightLive runs the post-install pong round against the running binary.
func preflightLive(reg *providers.Registry) int {
	bin, err := os.Executable()
	if err != nil {
		return helptext.Fail("preflight: %v", err)
	}
	preflight.RunInstallStep(context.Background(), os.Stdout, reg, bin, time.Now())
	return 0
}

// cmdPreflight is the hidden `preflight [--live|--refresh]`: pin rows, the post-install step, or a silent re-list.
func cmdPreflight(args []string) int {
	fs := helptext.NewFlagSet("preflight", helptext.Page{
		Usage: "claude-code preflight [--live|--refresh|--discover <profile>]",
		About: "Print each agent's pin check; --live also refreshes stale listings and runs the post-install pong round.\n" +
			"--refresh only re-lists stale listings, silently.\n" +
			"--discover re-seals one profile's model snapshot (Databricks or github), silently (a launch spawns it when expired).",
	})
	live := fs.Bool("live", false, "refresh stale listings and ask each pinned agent for pong")
	refresh := fs.Bool("refresh", false, "re-list stale served-endpoint caches, print nothing")
	discover := fs.String("discover", "", "re-seal one profile's model snapshot (Databricks or github)")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if *discover != "" {
		ctx, cancel := context.WithTimeout(context.Background(), preflight.Budget)
		defer cancel()
		if err := copilot.RefreshProfile(ctx, *discover); err != nil {
			return helptext.Fail("preflight: %v", err)
		}
		return 0
	}
	reg, err := providers.Load()
	if err != nil {
		return helptext.Fail("preflight: %v", err)
	}
	if *refresh {
		preflightRefresh(reg)
		return 0
	}
	if *live {
		return preflightLive(reg)
	}
	for _, p := range preflight.Check(reg, time.Now()) {
		fmt.Printf("%-8s %-9s %s\n", p.Agent, p.Status, p.Line())
	}
	return 0
}

// printAudit prints the typed action log: switches, pins, secret sets, safety blocks, proxy
// failovers. Shared by `doctor --audit`.
func printAudit(days int, format string) int {
	if days <= 0 {
		helptext.Fprint(os.Stderr, "usage: claude-code doctor --audit [--days 7] [--format text|json]\n")
		return 2
	}
	ev, err := audit.Read(time.Now().AddDate(0, 0, -days))
	if err != nil {
		return helptext.Fail("doctor --audit: %v", err)
	}
	if format == helptext.JSON {
		if err := helptext.WriteJSON(os.Stdout, struct {
			Days    int                  `json:"days"`
			Summary map[audit.Action]int `json:"summary"`
			Events  []audit.Event        `json:"events"`
		}{days, audit.Summary(ev), ev}); err != nil {
			return helptext.Fail("doctor --audit: %v", err)
		}
		return 0
	}
	if len(ev) == 0 {
		fmt.Printf("no audited actions in the last %d day(s) (%s)\n", days, audit.File())
		return 0
	}
	for _, e := range ev {
		fmt.Printf("%s  %-16s %-22s %s\n", e.Time.Local().Format("2006-01-02 15:04"), e.Action, e.Resource, e.Detail)
	}
	return 0
}
