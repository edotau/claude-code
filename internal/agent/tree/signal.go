// signal.go derives a running dispatch's own sign of life, so a quiet relay row is not read as a stalled child.

package tree

import (
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
)

// quietAfter is how long a running dispatch may go without an event or log write before its row reads "quiet".
var quietAfter = 5 * time.Minute

// applySignal stamps a running dispatch with its last sign of life and whether that is older than quietAfter.
func applySignal(d Dispatch, rec agent.Record, now time.Time) Dispatch {
	if rec.Status != statusRunning {
		return d
	}
	if !rec.Alive() {
		d.Status = "lost"
		return d
	}
	d.LastSignal, d.SignalSource = agent.LastSignal(rec)
	if d.Live = rec.QueueLine(now); d.Live != "" { // waiting for serve admission is not silence
		d.queued, d.queuedAt = queueMark(rec.Progress)
		return d
	}
	d.Quiet = now.Sub(d.LastSignal) > quietAfter
	d.Live = agent.ProgressOf(rec, now).Fragment(80)
	return d
}

// queueMark reads the "queued <RFC3339Nano> <pool>" mark that QueueLine has already validated.
func queueMark(progress string) (pool string, since time.Time) {
	stamp, pool, _ := strings.Cut(strings.TrimPrefix(progress, "queued "), " ")
	since, _ = time.Parse(time.RFC3339Nano, stamp)
	return pool, since
}
