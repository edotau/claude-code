package agenttree

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/paths"
)

// scanAllRecords is RecentRecords' n for "every record": the log itself is capped by the dispatch keep.
const scanAllRecords = 1 << 20

// markerLockWait bounds the wait for the agent-inflight lock, as the hook's writers do.
const markerLockWait = 2 * time.Second

// lostError is the Error a reaped dispatch record carries.
const lostError = "lost: process gone"

// RunningDispatches lists live dispatch jobs machine-wide (every repo, any age), newest first.
func RunningDispatches(now time.Time) []Leg {
	var legs []Leg
	for _, rec := range agent.RecentRecords(scanAllRecords) {
		if !rec.Alive() {
			continue
		}
		leg := legFromRecord(rec, "", now)
		leg.Repo = rec.Repo
		legs = append(legs, leg)
	}
	return legs
}

// Reap marks dispatch records that say running but whose job is gone as error, and removes dead or stale
// agent-inflight markers; dryRun only prints. It returns 1 when the marker lock could not be taken or a save failed.
func Reap(dryRun bool, out, errw io.Writer) int {
	verb := "reaped"
	if dryRun {
		verb = "would reap"
	}
	code := 0
	records := 0
	for _, rec := range agent.RecentRecords(scanAllRecords) {
		if rec.Status != "running" || rec.Alive() {
			continue
		}
		rec.Status, rec.Error = "error", lostError
		if !dryRun {
			if err := rec.Save(); err != nil {
				fmt.Fprintf(errw, "agents reap: %s: %v\n", rec.ID, err)
				code = 1
				continue
			}
		}
		fmt.Fprintf(out, "%s dispatch %s (%s, pid %d)\n", verb, rec.ID, rec.Agent, rec.PID)
		records++
	}
	markers, err := reapMarkers(dryRun, verb, out)
	if err != nil {
		fmt.Fprintln(errw, "agents reap:", err)
		code = 1
	}
	if records+markers == 0 {
		fmt.Fprintln(out, "agents reap: nothing to reap")
		return code
	}
	fmt.Fprintf(out, "%s %d dispatches, %d markers\n", verb, records, markers)
	return code
}

// reapMarkers removes the markers the hook's own liveness rule would prune: a dead owner, or an expired
// TTL where the owner cannot be probed. Non-marker entries (the lock file) are never touched.
func reapMarkers(dryRun bool, verb string, out io.Writer) (int, error) {
	dir := inflight.Dir()
	if _, err := os.Stat(dir); err != nil {
		return 0, nil // no marker dir yet: nothing was ever dispatched
	}
	n := 0
	sweep := func() error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		trusted, now := inflight.ProbeTrusted(), time.Now()
		for _, e := range entries {
			if _, ok := inflight.Owner(e.Name()); !ok {
				continue
			}
			fi, err := e.Info()
			if err != nil || inflight.Live(e.Name(), fi.ModTime(), now, 0, "", trusted) {
				continue
			}
			if !dryRun {
				if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
					continue
				}
			}
			fmt.Fprintf(out, "%s marker %s\n", verb, e.Name())
			n++
		}
		return nil
	}
	if dryRun {
		return n, sweep()
	}
	ran, err := paths.TryFileLock(filepath.Join(dir, ".lock"), markerLockWait, sweep)
	if err == nil && !ran {
		err = fmt.Errorf("agent-inflight marker dir is locked; markers skipped")
	}
	return n, err
}
