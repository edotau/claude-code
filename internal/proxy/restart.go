package proxy

import (
	"context"
	"fmt"
)

// RestartRow is one daemon's outcome in `network restart`.
type RestartRow struct {
	Daemon string `json:"daemon"`
	Status string `json:"status"` // restarted | would restart | skipped | failed
	OldPID int    `json:"old_pid,omitempty"`
	NewPID int    `json:"new_pid,omitempty"`
	Note   string `json:"note,omitempty"`
}

// RestartReport restarts the running proxy (unless dry, or requests are in flight and not force) and reports the outcome.
func RestartReport(ctx context.Context, dry, force bool) RestartRow {
	row := RestartRow{Daemon: "proxy"}
	st, ok := Running(ctx)
	if !ok {
		row.Status, row.Note = "skipped", "not running"
		return row
	}
	if !force {
		if n := Inflight(ctx, st.Port); n > 0 {
			row.Status, row.OldPID = "skipped", st.PID
			row.Note = fmt.Sprintf("requests in flight: %d; --force restarts anyway", n)
			return row
		}
	}
	if dry {
		row.Status, row.OldPID = "would restart", st.PID
		return row
	}
	old, cur, restarted, err := Restart(ctx)
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
