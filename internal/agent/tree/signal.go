// signal.go derives a running dispatch's own sign of life, so a quiet relay row is not read as a stalled child.

package tree

import (
	"fmt"
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
	d.Quiet = now.Sub(d.LastSignal) > quietAfter
	d.Live = agent.ProgressOf(rec, now).Line(80)
	return d
}

// signalText is the row's liveness column: "⚠ quiet 7m" past the threshold, else the progress line, "" when not running.
func signalText(d Dispatch, now time.Time) string {
	if d.LastSignal.IsZero() {
		return ""
	}
	if d.Quiet {
		return "⚠ quiet " + age(d.LastSignal, now)
	}
	if d.Live != "" {
		return d.Live
	}
	return fmt.Sprintf("last=%s", max(now.Sub(d.LastSignal), 0).Round(time.Second))
}
