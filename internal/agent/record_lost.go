package agent

import "github.com/edotau/claude-code/internal/proc"

// LostError opens the error text of a dispatch whose process vanished.
const LostError = "lost: process gone"

// processGone: a recorded pid that is dead, or alive with another start time (reused); no pid or start fails closed.
func (r Record) processGone() bool {
	if r.Status != "running" || r.PID <= 0 {
		return false
	}
	if !r.Alive() {
		return true
	}
	if r.PIDStart == "" {
		return false
	}
	got, err := proc.StartTime(r.PID)
	return err == nil && got != r.PIDStart
}

// reapLost marks a running record whose process is gone as lost, saved under its lock; any other record is returned as is.
func reapLost(r Record) Record {
	if !r.processGone() {
		return r
	}
	cur, _, err := updateRecord(r.ID, func(c Record) (Record, bool) {
		if !c.processGone() {
			return c, false
		}
		c.Status, c.Error = "lost", LostError+c.LostProcessDetail()
		return c, true
	})
	if err != nil {
		return r
	}
	return cur
}
