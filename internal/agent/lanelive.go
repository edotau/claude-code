package agent

import (
	"errors"
	"io/fs"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
)

// RecordLive reports whether rec's job is still running, reloading its record so a resumed PID counts.
func RecordLive(rec Record) bool {
	if r, err := LoadRecord(rec.ID); err == nil { // a pruned record falls back to the one given
		rec = r
	}
	if rec.PID <= 0 || !proc.Alive(rec.PID) {
		return false
	}
	got, err := proc.StartTime(rec.PID)
	return rec.PIDStart == "" || err != nil || got == rec.PIDStart // another start time is a reused pid
}

// LiveJobIn is the first running job outside the caller's ancestry (skip) whose tree is inside lane and is alive.
func LiveJobIn(lane string, skip map[int]bool) (Record, bool, error) {
	recs, err := RunningRecent(AllRecords)
	for _, r := range recs {
		if r.Status == "running" && r.Tree != "" && !skip[r.PID] && paths.InsideDir(r.Tree, lane) && jobLive(r) {
			return r, true, nil
		}
	}
	if errors.Is(err, fs.ErrNotExist) {
		err = nil // no dispatch dir yet: no jobs
	}
	return Record{}, false, err
}

// jobLive is RecordLive, except a pid-less record counts as live for its first minute.
func jobLive(r Record) bool {
	if r.PID <= 0 {
		return r.Alive()
	}
	return RecordLive(r)
}
