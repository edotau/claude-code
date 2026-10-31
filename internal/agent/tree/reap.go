package tree

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc/inflight"
)

// scanAllRecords is RecentRecords' n for "every record": the log itself is capped by the dispatch keep.
const scanAllRecords = 1 << 20

// markerLockWait bounds the wait for the agent-inflight lock, as the hook's writers do.
const markerLockWait = 2 * time.Second

// lostError is the Error a reaped dispatch record carries.
const lostError = "lost: process gone"

// RunningDispatches lists live dispatch jobs machine-wide (every repo, any age), newest first.
func RunningDispatches(now time.Time) []Dispatch {
	var dispatches []Dispatch
	for _, rec := range agent.RecentRecords(scanAllRecords) {
		if !rec.Alive() {
			continue
		}
		disp := dispatchFromRecord(rec, "", now)
		disp.Repo = rec.Repo
		disp.sessionID, disp.cwd, disp.started, disp.prompt = rec.SessionID, rec.Request.WorkDir, rec.Time, rec.Request.Prompt
		if disp.cwd == "" {
			disp.cwd = rec.Repo
		}
		dispatches = append(dispatches, disp)
	}
	return dispatches
}

// dropDispatchSessions returns sessions minus bare rows that duplicate a running headless claude dispatch.
func dropDispatchSessions(sessions []Session, dispatches []Dispatch) []Session {
	out := make([]Session, 0, len(sessions))
	for _, s := range sessions {
		if len(s.Subagents) == 0 && len(s.Workflows) == 0 && matchesClaudeDispatch(s, dispatches) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// matchesClaudeDispatch reports whether s is the session a running claude dispatch spawned (by id, else by cwd+time).
func matchesClaudeDispatch(s Session, dispatches []Dispatch) bool {
	for _, l := range dispatches {
		if s, ok := agent.LookupSchema(l.Agent); !ok || s.External() { // not the Claude launcher
			continue
		}
		if l.sessionID != "" && l.sessionID == s.ID {
			return true
		}
		if l.cwd != "" && l.cwd == s.Cwd && !s.Updated.Before(l.started) && s.Title != "" &&
			strings.HasPrefix(oneLine(strings.TrimSpace(l.prompt)), strings.TrimSuffix(s.Title, "…")) {
			return true
		}
	}
	return false
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
	socks := reapSockets(dryRun, verb, out)
	markers, err := reapMarkers(dryRun, verb, out)
	if err != nil {
		fmt.Fprintln(errw, "agents reap:", err)
		code = 1
	}
	markers += socks
	if records+markers == 0 {
		fmt.Fprintln(out, "agents reap: nothing to reap")
		return code
	}
	fmt.Fprintf(out, "%s %d dispatches, %d markers\n", verb, records, markers)
	return code
}

// reapSockets removes live-session sockets whose holder is gone (no record, or not alive); a live holder keeps its own.
func reapSockets(dryRun bool, verb string, out io.Writer) int {
	socks, _ := filepath.Glob(filepath.Join(agent.DispatchDir(), "*"+agent.LiveSockSuffix))
	n := 0
	for _, sock := range socks {
		id := strings.TrimSuffix(filepath.Base(sock), agent.LiveSockSuffix)
		if rec, err := agent.LoadRecord(id); err == nil && rec.Alive() {
			continue
		}
		if !dryRun && os.Remove(sock) != nil {
			continue
		}
		fmt.Fprintf(out, "%s socket %s\n", verb, filepath.Base(sock))
		n++
	}
	return n
}

// reapMarkers removes the markers the limiter's own liveness rule would prune (inflight.Liveness).
// Non-marker entries (the lock file) are never touched.
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
		live := inflight.Liveness(time.Now())
		for _, e := range entries {
			if _, ok := inflight.Owner(e.Name()); !ok || live(e.Name()) {
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
		err = errors.New("agent-inflight marker dir is locked; markers skipped")
	}
	return n, err
}
