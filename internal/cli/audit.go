package cli

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/outfmt"
)

const auditUsage = "usage: claude-code audit [--days 7] [--format text|json]"

// cmdAudit prints the typed action log: switches, pins, secret sets, safety blocks, router failovers.
func cmdAudit(args []string) int {
	fs := flag.NewFlagSet("audit", flag.ContinueOnError)
	days := fs.Int("days", 7, "events from the last N days")
	format := outfmt.Flag(fs)
	if fs.Parse(args) != nil {
		return 2
	}
	if err := outfmt.Check(*format); err != nil || *days <= 0 {
		fmt.Fprintln(os.Stderr, auditUsage)
		return 2
	}
	ev, err := audit.Read(time.Now().AddDate(0, 0, -*days))
	if err != nil {
		return fail("audit: %v", err)
	}
	if *format == outfmt.JSON {
		_ = outfmt.Write(os.Stdout, struct {
			Days    int                  `json:"days"`
			Summary map[audit.Action]int `json:"summary"`
			Events  []audit.Event        `json:"events"`
		}{*days, audit.Summary(ev), ev})
		return 0
	}
	if len(ev) == 0 {
		fmt.Printf("no audited actions in the last %d day(s) (%s)\n", *days, audit.File())
		return 0
	}
	for _, e := range ev {
		fmt.Printf("%s  %-16s %-22s %s\n", e.Time.Local().Format("2006-01-02 15:04"), e.Action, e.Resource, e.Detail)
	}
	return 0
}
