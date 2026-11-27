package agent

import (
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/proc"
)

// LostError opens the error text of a dispatch whose process vanished.
const LostError = "lost: process gone"

// processGone: a recorded pid that is dead, or alive with another start time (reused); no pid or start fails closed.
func (r Record) processGone() bool {
	if r.Status != "running" || r.PID <= 0 {
		return false
	}
	return !identityLive(r.PID, r.PIDStart)
}

// reapLost marks a dead running record as lost under its lock.
func reapLost(r Record) Record {
	if !r.processGone() {
		return r
	}
	cur, _, err := updateRecord(r.ID, func(c Record) (Record, bool) {
		if !c.processGone() {
			return c, false
		}
		c.Status, c.Error = "lost", LostError+c.LostProcessDetail()
		if c.PPID > 0 {
			if rc := proc.ExitReceipt(paths.MemoryStateDir(), c.PPID); rc != "" {
				c.Error += "; worker " + rc
			}
		}
		if !c.cancelled() { // a deliberate cancel is no failed attempt
			c.NoteHealth(nil) // a lost run is a failed attempt, counted before its record is visible
		}
		return c, true
	})
	if err != nil {
		return r
	}
	return cur
}
