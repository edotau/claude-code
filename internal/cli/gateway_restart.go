package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/edotau/claude-code/internal/egress"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// proxyRestartReport is `gateway proxy --restart` with --if-changed/--force/--dry-run: a stopped proxy is skipped.
func proxyRestartReport(o *proxyOpts, format string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	row := proxy.RestartReport(ctx, o.dry, o.force, o.ifChanged)
	rows := []proxy.RestartRow{row}
	if row.Status == "restarted" || row.Status == "would restart" || row.Status == "failed" {
		rows = append(rows, stopTunnels(o.dry)...)
	}
	if format == helptext.JSON {
		if err := helptext.WriteJSON(os.Stdout, map[string]any{"daemons": rows}); err != nil {
			return helptext.Fail("gateway proxy: %v", err)
		}
	}
	code := 0
	for _, r := range rows {
		if format != helptext.JSON {
			printRestartRow(r)
		}
		if r.Status == "failed" {
			code = 1
		}
	}
	return code
}

// stopTunnels ends every recorded --remote ssh tunnel (dry: reports them), one row per egress host.
func stopTunnels(dry bool) []proxy.RestartRow {
	var rows []proxy.RestartRow
	for _, t := range egress.Tunnels() {
		row := proxy.RestartRow{Daemon: "egress", Status: "stopped", OldPID: t.PID, Note: t.Host}
		if dry {
			row.Status = "would stop"
		} else if err := egress.Stop(t.Host); err != nil {
			row.Status, row.Note = "failed", err.Error()
		}
		rows = append(rows, row)
	}
	return rows
}

func printRestartRow(r proxy.RestartRow) {
	switch r.Status {
	case "restarted":
		fmt.Printf("%-8s restarted  pid %d → %d\n", r.Daemon, r.OldPID, r.NewPID)
	case "unchanged":
		fmt.Printf("%-8s unchanged  pid %d\n", r.Daemon, r.OldPID)
	case "would restart":
		fmt.Printf("%-8s would restart  pid %d\n", r.Daemon, r.OldPID)
	case "stopped", "would stop":
		fmt.Printf("%-8s %s  %s pid %d\n", r.Daemon, r.Status, r.Note, r.OldPID)
	case "skipped":
		fmt.Printf("%-8s %s (skipped)\n", r.Daemon, r.Note)
	default:
		fmt.Fprintf(os.Stderr, "%-8s restart failed: %s\n", r.Daemon, r.Note)
	}
}
