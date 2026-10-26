package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/outfmt"
	"github.com/edotau/claude-code/internal/router"
)

var restartPage = helptext.Page{
	Usage: "claude-code network restart [--dry-run] [--format text|json]",
	About: "Restart each running harness daemon on the installed binary. Stopped\n" +
		"daemons are skipped. Covers the router; the memory idle watcher is\n" +
		"per-session and exits by itself, so it is not restarted.",
}

// restartRow is one daemon's outcome in `restart`.
type restartRow struct {
	Daemon string `json:"daemon"`
	Status string `json:"status"` // restarted | would restart | skipped | failed
	OldPID int    `json:"old_pid,omitempty"`
	NewPID int    `json:"new_pid,omitempty"`
	Note   string `json:"note,omitempty"`
}

func cmdRestart(args []string) int {
	fs := helptext.NewFlagSet("network restart", restartPage)
	dry := fs.Bool("dry-run", false, "report what would restart; change nothing")
	format := outfmt.Flag(fs)
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if err := outfmt.Check(*format); err != nil {
		return fail("restart: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	row := restartRouter(ctx, *dry)
	if *format == outfmt.JSON {
		if err := outfmt.Write(os.Stdout, map[string]any{"daemons": []restartRow{row}}); err != nil {
			return fail("restart: %v", err)
		}
	} else {
		printRestartRow(row)
	}
	if row.Status == "failed" {
		return 1
	}
	return 0
}

func restartRouter(ctx context.Context, dry bool) restartRow {
	row := restartRow{Daemon: "router"}
	if dry {
		st, ok := router.Running(ctx)
		if !ok {
			row.Status, row.Note = "skipped", "not running"
			return row
		}
		row.Status, row.OldPID = "would restart", st.PID
		return row
	}
	old, cur, restarted, err := router.Restart(ctx)
	switch {
	case err != nil:
		row.Status, row.OldPID, row.Note = "failed", old.PID, err.Error()
	case !restarted:
		row.Status, row.Note = "skipped", "not running"
	default:
		row.Status, row.OldPID, row.NewPID = "restarted", old.PID, cur.PID
	}
	return row
}

func printRestartRow(r restartRow) {
	switch r.Status {
	case "restarted":
		fmt.Printf("%-8s restarted  pid %d → %d\n", r.Daemon, r.OldPID, r.NewPID)
	case "would restart":
		fmt.Printf("%-8s would restart  pid %d\n", r.Daemon, r.OldPID)
	case "skipped":
		fmt.Printf("%-8s %s (skipped)\n", r.Daemon, r.Note)
	default:
		fmt.Fprintf(os.Stderr, "%-8s restart failed: %s\n", r.Daemon, r.Note)
	}
}
