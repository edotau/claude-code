package tree

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent/job"
	"github.com/edotau/claude-code/internal/agent/wire"

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
func RunningDispatches(now time.Time) []Dispatch {
	var dispatches []Dispatch
	for _, rec := range job.RecentRecords(scanAllRecords) {
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
		if l.sessionID != "" { // a known id decides alone: a title match must not claim another session
			if l.sessionID == s.ID {
				return true
			}
			continue
		}
		if matchesDispatchTitle(s, l) {
			return true
		}
	}
	return false
}

func matchesDispatchTitle(s Session, l Dispatch) bool {
	return l.cwd != "" && l.cwd == s.Cwd && !s.Updated.Before(l.started) && s.Title != "" &&
		strings.HasPrefix(oneLine(strings.TrimSpace(l.prompt)), strings.TrimSuffix(s.Title, "…"))
}

// Reap errors out lost running dispatches and removes dead inflight markers; dryRun only prints. Returns 1 on failure.
func Reap(dryRun bool, out, errw io.Writer) int {
	verb := "reaped"
	if dryRun {
		verb = "would reap"
	}
	snapshot, err := job.ReadRecent(scanAllRecords)
	if err != nil {
		fmt.Fprintln(errw, "agents reap:", err)
		return 1
	}
	records, code := reapRecords(snapshot, dryRun, verb, out, errw)
	socks, sockCode := reapSockets(dryRun, verb, out, errw)
	markers, err := reapMarkers(dryRun, verb, out)
	if err != nil {
		fmt.Fprintln(errw, "agents reap:", err)
		code = 1
	}
	code = max(code, sockCode)
	if records+markers+socks == 0 {
		fmt.Fprintln(out, "agents reap: nothing to reap")
		return code
	}
	fmt.Fprintf(out, "%s %d dispatches, %d markers, %d sockets\n", verb, records, markers, socks)
	return code
}

func reapRecords(snapshot []job.Record, dryRun bool, verb string, out, errw io.Writer) (int, int) {
	code, records := 0, 0
	for _, rec := range snapshot {
		if rec.Status != "running" || rec.Alive() {
			continue
		}
		cur, err := job.LoadRecord(rec.ID)
		if err != nil || cur.Status != "running" || cur.Alive() {
			continue // the owner may have finished or started after the snapshot
		}
		rec = cur
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
	return records, code
}

// reapSockets removes sockets whose holder is dead or has no dispatch record; returns the count and 1 on a failed remove.
func reapSockets(dryRun bool, verb string, out, errw io.Writer) (int, int) {
	socks, _ := filepath.Glob(filepath.Join(paths.DispatchDir(), "*"+wire.LiveSockSuffix))
	n, code := 0, 0
	for _, sock := range socks {
		id := strings.TrimSuffix(filepath.Base(sock), wire.LiveSockSuffix)
		if socketHolderAlive(id) {
			continue
		}
		if !dryRun {
			if err := os.Remove(sock); err != nil {
				if !errors.Is(err, fs.ErrNotExist) {
					fmt.Fprintf(errw, "agents reap: socket %s: %v\n", filepath.Base(sock), err)
					code = 1
				}
				continue
			}
		}
		fmt.Fprintf(out, "%s socket %s\n", verb, filepath.Base(sock))
		n++
	}
	return n, code
}

// socketHolderAlive reads id's record at call time, so a session started during the reap keeps its socket.
func socketHolderAlive(id string) bool {
	rec, err := job.LoadRecord(id)
	return err == nil && rec.Alive()
}

// reapMarkers removes the markers inflight.Liveness would prune; non-marker entries (the lock file) stay.
func reapMarkers(dryRun bool, verb string, out io.Writer) (int, error) {
	dir := inflight.Dir()
	if _, err := os.Stat(dir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil // no marker dir yet: nothing was ever dispatched
		}
		return 0, fmt.Errorf("marker dir: %w", err)
	}
	if dryRun {
		return sweepMarkers(dir, dryRun, verb, out)
	}
	n := 0
	sweep := func() error {
		var err error
		n, err = sweepMarkers(dir, dryRun, verb, out)
		return err
	}
	ran, err := paths.TryFileLock(filepath.Join(dir, ".lock"), markerLockWait, sweep)
	if err == nil && !ran {
		err = errors.New("agent-inflight marker dir is locked; markers skipped")
	}
	return n, err
}

func sweepMarkers(dir string, dryRun bool, verb string, out io.Writer) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	live := inflight.Liveness(time.Now())
	n := 0
	var errs []error
	for _, e := range entries {
		if _, ok := inflight.Owner(e.Name()); !ok || live(e.Name()) {
			continue
		}
		if !dryRun {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
				if !errors.Is(err, fs.ErrNotExist) {
					errs = append(errs, fmt.Errorf("marker %s: %w", e.Name(), err))
				}
				continue
			}
		}
		fmt.Fprintf(out, "%s marker %s\n", verb, e.Name())
		n++
	}
	return n, errors.Join(errs...)
}
