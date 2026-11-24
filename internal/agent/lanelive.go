package agent

import (
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
)

// RecordLive reports whether rec's job is still running, reloading its record so a resumed PID counts.
func RecordLive(rec Record) bool {
	if r, err := LoadRecord(rec.ID); err == nil { // a pruned record falls back to the one given
		rec = r
	}
	return rec.PID > 0 && proc.Alive(rec.PID)
}

// LiveJobIn is the first running dispatch job whose tree lies inside lane and whose process is alive.
func LiveJobIn(lane string) (Record, bool) {
	recs, _ := RunningRecent(AllRecords)
	want := paths.CanonDeep(lane)
	for _, r := range recs {
		if r.Tree != "" && paths.Within(paths.CanonDeep(r.Tree), want) && RecordLive(r) {
			return r, true
		}
	}
	return Record{}, false
}
