package audit

import (
	"fmt"
	"os"
	"time"

	"github.com/edotau/claude-code/internal/ui/helptext"
)

// Report prints the typed action log (switches, pins, secret sets, safety blocks, failovers) for `doctor --audit`.
func Report(days int, format string) int {
	if days <= 0 {
		helptext.Fprint(os.Stderr, "usage: claude-code doctor --audit [--days 7] [--format text|json]\n")
		return 2
	}
	ev, err := Read(time.Now().AddDate(0, 0, -days))
	if err != nil {
		return helptext.Fail("doctor --audit: %v", err)
	}
	if format == helptext.JSON {
		if err := helptext.WriteJSON(os.Stdout, struct {
			Days    int            `json:"days"`
			Summary map[Action]int `json:"summary"`
			Events  []Event        `json:"events"`
		}{days, Summary(ev), ev}); err != nil {
			return helptext.Fail("doctor --audit: %v", err)
		}
		return 0
	}
	if len(ev) == 0 {
		fmt.Printf("no audited actions in the last %d day(s) (%s)\n", days, File())
		return 0
	}
	for _, e := range ev {
		fmt.Printf("%s  %-16s %-22s %s\n", e.Time.Local().Format("2006-01-02 15:04"), e.Action, e.Resource, e.Detail)
	}
	return 0
}
