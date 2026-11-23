package agent

import (
	"bytes"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
)

// LostError opens the error text of a dispatch whose process vanished.
const LostError = "lost: process gone"

// recoverMaxAge bounds how far before boot a job may have started and still be relaunched; tests shrink it.
var recoverMaxAge = 24 * time.Hour

// recoveryMark is written to a job's log at relaunch, so a job that dies again is never relaunched twice.
const recoveryMark = "agents serve: relaunched after reboot\n"

// reviewSummary is the prompt of a `<agent> review --background` record, which is not a replayable ask.
var reviewSummary = regexp.MustCompile(`^\S+ review split into \d+ parts$`)

// relaunchable is true only for a top-level plain `agents ask --background` record whose flags the record can replay.
func (r Record) relaunchable() bool {
	return r.Background && !r.Live && r.Host == "" && r.Parent == "" && !r.Request.Carry && r.Agent != "waves" &&
		r.Request.Prompt != "" && r.FallbackPolicy != "" && !reviewSummary.MatchString(r.Request.Prompt)
}

func (r Record) recoverable(boot time.Time) bool {
	if !r.relaunchable() || !r.Time.Before(boot) || r.Time.Before(boot.Add(-recoverMaxAge)) || r.Time.IsZero() {
		return false
	}
	if r.Status == "lost" && strings.HasPrefix(r.Error, LostError) {
		r.Status = "running"
	}
	if !r.processGone() {
		return false
	}
	log, err := os.ReadFile(r.LogPath())
	return err != nil || !bytes.Contains(log, []byte(recoveryMark))
}

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
		return c, true
	})
	if err != nil {
		return r
	}
	return cur
}
