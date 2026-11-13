// signal.go derives a running dispatch's own sign of life, so a quiet relay row is not read as a stalled child.

package tree

import (
	"fmt"
	"os"
	"time"

	"github.com/edotau/claude-code/internal/agent"
)

// quietAfter is how long a running dispatch may go without an event or log write before its row reads "quiet".
var quietAfter = 5 * time.Minute

// lastSignal is the newest mtime of the dispatch's event file or log (two stats), else the record's start time.
func lastSignal(rec agent.Record) (time.Time, string) {
	at, source := rec.Time, "record"
	for _, c := range []struct{ path, name string }{{rec.EventsPath(), "events"}, {rec.LogPath(), "log"}} {
		if info, err := os.Stat(c.path); err == nil && info.ModTime().After(at) {
			at, source = info.ModTime(), c.name
		}
	}
	return at, source
}

// applySignal stamps a running dispatch with its last sign of life and whether that is older than quietAfter.
func applySignal(d Dispatch, rec agent.Record, now time.Time) Dispatch {
	if rec.Status != statusRunning {
		return d
	}
	if !rec.Alive() {
		d.Status = "lost"
		return d
	}
	d.LastSignal, d.SignalSource = lastSignal(rec)
	d.Quiet = now.Sub(d.LastSignal) > quietAfter
	return d
}

// signalText is the row's liveness column: "⚠ quiet 7m" past the threshold, "last=12s" otherwise, "" when not running.
func signalText(d Dispatch, now time.Time) string {
	if d.LastSignal.IsZero() {
		return ""
	}
	if d.Quiet {
		return "⚠ quiet " + age(d.LastSignal, now)
	}
	return fmt.Sprintf("last=%s", max(now.Sub(d.LastSignal), 0).Round(time.Second))
}
