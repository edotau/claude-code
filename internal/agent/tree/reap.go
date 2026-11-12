package tree

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
)

// lostError is the Error a reaped dispatch record carries.
const lostError = "lost: process gone"

// runningDispatches lists live dispatch jobs machine-wide (every repo, any age), newest first.
func runningDispatches(now time.Time) []Dispatch {
	var dispatches []Dispatch
	for _, rec := range agent.RecentRecords(agent.AllRecords) {
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

// Reap errors out lost running dispatches, removes dead inflight markers and stops orphaned dispatch children;
// dryRun only prints. Returns 1 on failure.
func Reap(dryRun bool, out, errw io.Writer) int {
	verb := "reaped"
	if dryRun {
		verb = "would reap"
	}
	snapshot, err := agent.ReadRecent(agent.AllRecords)
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
	orphans, orphanCode := reapOrphans(dryRun, verb, out, errw)
	code = max(code, sockCode, orphanCode)
	if records+markers+socks+orphans == 0 {
		fmt.Fprintln(out, "agents reap: nothing to reap")
		return code
	}
	fmt.Fprintf(out, "%s %d dispatches, %d markers, %d sockets, %d orphans\n", verb, records, markers, socks, orphans)
	return code
}

func reapRecords(snapshot []agent.Record, dryRun bool, verb string, out, errw io.Writer) (int, int) {
	code, records := 0, 0
	for _, rec := range snapshot {
		if rec.Status != "running" || rec.Alive() {
			continue
		}
		cur, err := agent.LoadRecord(rec.ID)
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

// Seams for tests: the process table, the group signal, the liveness probe, the clock and the SIGTERM-to-SIGKILL wait.
var (
	listProcs   = proc.List
	signalGroup = proc.SignalGroup
	procAlive   = proc.Alive
	reapNow     = time.Now
	reapSleep   = time.Sleep
)

// orphanGrace is how long a SIGTERMed orphan gets before its group is killed.
const orphanGrace = 3 * time.Second

// orphanSlack is how far past its dispatch deadline a child may run before it counts as leaked.
const orphanSlack = time.Minute

// reapOrphans stops the children orphanedChildren finds; returns their count and 1 on a failed signal or listing.
func reapOrphans(dryRun bool, verb string, out, errw io.Writer) (int, int) {
	procs, err := listProcs(proc.DispatchIDEnv)
	if err != nil {
		fmt.Fprintf(errw, "agents reap: process table: %v\n", err)
		return 0, 1
	}
	now := reapNow()
	n, code := 0, 0
	for _, o := range orphanedChildren(procs, now) {
		if !dryRun {
			if err := stopGroup(o.PID); err != nil {
				fmt.Fprintf(errw, "agents reap: orphan pid %d: %v\n", o.PID, err)
				code = 1
				continue
			}
		}
		fmt.Fprintf(out, "%s orphan %s pid %d (dispatch %s, up %s)\n", verb, o.Exe, o.PID, o.Mark,
			now.Sub(o.Start).Round(time.Second))
		n++
	}
	return n, code
}

// orphanedChildren picks the dispatch children (marked with the dispatch id, leading their own process group) whose
// dispatcher is gone and that outlived the dispatch's deadline; a hand-started process carries no mark.
func orphanedChildren(procs []proc.Info, now time.Time) []proc.Info {
	byPID := make(map[int]proc.Info, len(procs))
	for _, p := range procs {
		byPID[p.PID] = p
	}
	var out []proc.Info
	for _, p := range procs {
		if p.Mark == "" || p.PGID != p.PID || !parentGone(p, byPID) {
			continue
		}
		deadline, ok := orphanDeadline(p.Mark)
		if ok && now.Sub(p.Start) > deadline+orphanSlack {
			out = append(out, p)
		}
	}
	return out
}

// parentGone reports whether p's parent is init or a subreaper (it was reparented) or no longer exists.
func parentGone(p proc.Info, byPID map[int]proc.Info) bool {
	parent, found := byPID[p.PPID]
	return p.PPID <= 1 || !found || slices.Contains([]string{"init", "systemd", "launchd"}, parent.Exe)
}

// orphanDeadline is the longest a child of dispatch id may run (its attempt timeout); false while it is alive.
func orphanDeadline(id string) (time.Duration, bool) {
	rec, err := agent.LoadRecord(id)
	switch {
	case err != nil: // pruned: the child is older than the 200-record window
		return agent.DefaultToolsTimeout, true
	case rec.Alive():
		return 0, false
	case rec.Request.Timeout > 0:
		return rec.Request.Timeout, true
	case rec.Request.Tools:
		return agent.DefaultToolsTimeout, true
	}
	return retry.DefaultPolicy().AttemptTimeout, true
}

// stopGroup sends pid's process group SIGTERM, then SIGKILL after orphanGrace if its leader is still there.
func stopGroup(pid int) error {
	if err := signalGroup(pid, syscall.SIGTERM); err != nil {
		return err
	}
	reapSleep(orphanGrace)
	if !procAlive(pid) {
		return nil
	}
	return signalGroup(pid, syscall.SIGKILL)
}

// socketHolderAlive reads id's record at call time, so a session started during the reap keeps its socket.
func socketHolderAlive(id string) bool {
	rec, err := agent.LoadRecord(id)
	return err == nil && rec.Alive()
}

// reapMarkers prints the markers inflight.Sweep removed (or would remove) and returns their count.
func reapMarkers(dryRun bool, verb string, out io.Writer) (int, error) {
	removed, err := inflight.Sweep(dryRun)
	for _, name := range removed {
		fmt.Fprintf(out, "%s marker %s\n", verb, name)
	}
	return len(removed), err
}

// pendingAskMarker flags a Bash arg as a relay that launched `ask` and has not seen its exit line yet.
const pendingAskMarker = "ask --agent"

// firstTimestamp reads a transcript first record timestamp: the subagent start time, zero on failure.
func firstTimestamp(path string) time.Time {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}
	}
	defer f.Close()
	sc := newLineScanner(f)
	if sc.Scan() {
		var rec struct {
			Timestamp string `json:"timestamp"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) == nil {
			return parseRFC3339(rec.Timestamp)
		}
	}
	return time.Time{}
}

// dispatchFromRecord renders a dispatch record; a still-running record reports its elapsed time so far.
func dispatchFromRecord(rec agent.Record, matched string, now time.Time) Dispatch {
	duration := rec.Elapsed()
	if duration == "" && rec.Status == "running" {
		duration = now.Sub(rec.Time).Round(time.Second).String()
	}
	task, _, _ := strings.Cut(strings.TrimSpace(rec.Request.Prompt), "\n")
	return Dispatch{
		ID: rec.ID, Agent: rec.Agent, Provider: rec.Provider, Model: cmp.Or(rec.Model, rec.Request.Model),
		Status: rec.Status, Duration: duration, Fallback: rec.Fallback,
		FallbackReason: rec.FallbackReason, Matched: matched, Route: rec.Route, Task: task,
	}
}

// buildDispatches returns each "ask: dispatch <id>" the transcript shows, plus a heuristic match when `ask` is pending.
func buildDispatches(transcriptPath, cwd string, now time.Time, act Activity, ids []string) []Dispatch {
	var dispatches []Dispatch
	matched := map[string]bool{}
	for _, id := range ids {
		rec, err := agent.LoadRecord(id)
		if err != nil {
			continue
		}
		dispatches = append(dispatches, dispatchFromRecord(rec, "", now))
		matched[rec.ID] = true
	}
	if act.Tool == "Bash" && strings.Contains(act.Arg, pendingAskMarker) {
		if disp := pendingDispatch(transcriptPath, cwd, matched, now); disp != nil {
			dispatches = append(dispatches, *disp)
		}
	}
	return dispatches
}

// pendingDispatch matches the newest running dispatch for this cwd that started no earlier than the subagent.
func pendingDispatch(transcriptPath, cwd string, matched map[string]bool, now time.Time) *Dispatch {
	start := firstTimestamp(transcriptPath)
	for _, rec := range agent.RecentRecords(50) {
		if matched[rec.ID] || rec.Status != "running" || rec.Request.WorkDir != cwd {
			continue
		}
		if !start.IsZero() && rec.Time.Before(start) {
			continue
		}
		disp := dispatchFromRecord(rec, "heuristic", now)
		return &disp
	}
	return nil
}
