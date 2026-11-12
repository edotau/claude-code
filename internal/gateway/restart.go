package gateway

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
)

// RestartRow is one daemon's outcome in `network restart`.
type RestartRow struct {
	Daemon string `json:"daemon"`
	Status string `json:"status"` // restarted | would restart | stopped | would stop | skipped | failed
	OldPID int    `json:"old_pid,omitempty"`
	NewPID int    `json:"new_pid,omitempty"`
	Note   string `json:"note,omitempty"`
}

// RestartReport restarts the running proxy (unless dry, or busy without force) and reports the outcome.
func RestartReport(ctx context.Context, dry, force bool) RestartRow {
	row := RestartRow{Daemon: "proxy"}
	st, ok := Running(ctx)
	if !ok {
		row.Status, row.Note = "skipped", "not running"
		return row
	}
	if !force {
		// Holder's Inflight is 0 when unreadable, so a wedged proxy can still be restarted.
		if held, _ := Holder(ctx, st.Port); held.Inflight > 0 {
			row.Status, row.OldPID = "skipped", st.PID
			row.Note = fmt.Sprintf("requests in flight: %d; --force restarts anyway", held.Inflight)
			return row
		}
	}
	if dry {
		row.Status, row.OldPID = "would restart", st.PID
		return row
	}
	cur, err := restart(ctx, st)
	row.OldPID = st.PID
	if err != nil {
		row.Status, row.Note = "failed", err.Error()
		return row
	}
	row.Status, row.NewPID = "restarted", cur.PID
	return row
}

// restartRetry spaces watchdog restarts machine-wide, so every status line refresh in a down-proxy window spawns once.
const restartRetry = 15 * time.Second

// heldPath marks a deliberate `gateway proxy --stop`: the watchdog leaves the proxy down until the next start.
func heldPath() string { return filepath.Join(paths.StateDir(), "proxy.held") }

// Hold keeps the watchdog from restarting a deliberately stopped proxy; release (any start) lifts it.
func Hold() error { return os.WriteFile(heldPath(), nil, 0o600) }

// release lifts a Hold; a missing marker is not an error.
func release() {
	if err := os.Remove(heldPath()); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "proxy: release hold: %v\n", err)
	}
}

// claimRestart takes the watchdog restart slot; false while held or when another claim is within restartRetry.
func claimRestart(now time.Time) bool {
	path := filepath.Join(paths.StateDir(), "proxy.restart-claim")
	claimed, ran := false, false
	err := paths.WithFileLock(path, func() error { // check-and-stamp under one lock: concurrent refreshes claim once
		claimed, ran = claimRestartLocked(path, now), true
		return nil
	})
	if err != nil && !ran { // an unlockable state dir falls back to the unlocked check; the stamp below still gates it
		return claimRestartLocked(path, now)
	}
	return claimed
}

// claimRestartLocked is claimRestart's check-and-stamp; the caller holds the claim lock when it can.
func claimRestartLocked(path string, now time.Time) bool {
	if _, err := os.Stat(heldPath()); err == nil {
		return false
	}
	if fi, err := os.Stat(path); err == nil && now.Sub(fi.ModTime()) < restartRetry {
		return false
	}
	// No stamp, no throttle: an unrecordable claim must not spawn, or every status-line refresh would.
	return os.MkdirAll(filepath.Dir(path), 0o700) == nil && os.WriteFile(path, nil, 0o600) == nil
}

// SpawnStart starts `gateway proxy --start` detached; false when held, claimed within 15s, unstampable or unresolvable.
func SpawnStart() bool {
	if !claimRestart(time.Now()) {
		return false
	}
	exe, err := paths.SelfBinary()
	if err != nil {
		return false
	}
	return proc.SpawnDetached(exe, []string{"gateway", "proxy", "--start"}, "", nil)
}
