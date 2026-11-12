// The idle watcher is the final update that needs no SessionEnd, which a crash, kill or closed terminal never fires.
package memory

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/proc"
)

// UpdateIdleAfter: a transcript this quiet is an ended session. Poll and lifetime bound the watcher.
const (
	UpdateIdleAfter = 10 * time.Minute
	updateWatchLife = 24 * time.Hour
)

// updateWatchPoll is the mtime poll gap; a var so tests can tighten it.
var updateWatchPoll = time.Minute

func watchClaimPath(sessionID string) string {
	return filepath.Join(StateDir(), "watch-"+filepath.Base(sessionID))
}

// spawnIdleWatcher starts THIS session's watcher once: a claim file makes every later Stop a no-op.
func spawnIdleWatcher(sessionID, transcript, cwd string) {
	if sessionID == "" {
		return
	}
	claim := watchClaimPath(sessionID)
	if !claimWatcher(claim) {
		return // claimed by an earlier Stop (or the state dir is unwritable — the Stop gate still runs)
	}
	args := []string{"--session", sessionID, "--transcript", transcript, "--cwd", cwd,
		"--owner", strconv.Itoa(inflight.SessionOwner())}
	if !spawn.watcher(args, cwd) {
		_ = os.Remove(claim) // let the next Stop try again
	}
}

// claimWatcher creates the claim file; one naming a dead watcher pid is a killed watcher's leftover and is retaken.
func claimWatcher(claim string) bool {
	for range 2 {
		f, err := os.OpenFile(claim, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_ = f.Close()
			return true
		}
		if !watcherDead(claim) {
			return false
		}
		_ = os.Remove(claim)
	}
	return false
}

// watcherDead reports whether the claim names a pid that is gone; an empty claim is a watcher still starting.
func watcherDead(claim string) bool {
	b, err := os.ReadFile(claim)
	pid, perr := strconv.Atoi(strings.TrimSpace(string(b)))
	return err == nil && perr == nil && ownerDead(pid)
}

// WatchOptions are `memory watch`'s parsed flags; Stderr receives its messages.
type WatchOptions struct {
	Session, Transcript, CWD string
	Idle                     time.Duration
	Owner                    int
	Stderr                   io.Writer
}

// IdleUpdateWatch is `claude-code memory watch`: the detached per-session watcher described above.
func IdleUpdateWatch(o WatchOptions) int {
	stderr := o.Stderr
	if o.Session == "" || o.Transcript == "" || o.Idle <= 0 {
		fmt.Fprintln(stderr, "memory watch: --session, --transcript and a positive --idle are required")
		return 2
	}
	if fi, err := os.Stat(o.CWD); err != nil || !fi.IsDir() {
		fmt.Fprintf(stderr, "memory watch: --cwd %q is not a directory\n", o.CWD)
		return 2
	}
	// The claim holds this pid so a later Stop retakes it after a kill; the defer frees it for a fresh watcher.
	_ = os.WriteFile(watchClaimPath(o.Session), []byte(strconv.Itoa(os.Getpid())), 0o600)
	defer func() { _ = os.Remove(watchClaimPath(o.Session)) }()
	w := &watchLoop{tr: o.Transcript, cwd: o.CWD, session: o.Session, idle: o.Idle, owner: o.Owner, stderr: stderr}
	deadline := time.Now().Add(updateWatchLife)
	for time.Now().Before(deadline) {
		time.Sleep(updateWatchPoll)
		if w.poll() {
			return 0
		}
	}
	return 0
}

// watchLoop is the watcher's per-session state across polls.
type watchLoop struct {
	tr, cwd, session string
	idle             time.Duration
	owner            int
	seen             time.Time // mtime last evaluated: the transcript is append-only, so unchanged ⇒ same edit count
	stderr           io.Writer
}

// poll runs one watch iteration and reports whether the watcher should exit.
func (w *watchLoop) poll() (exit bool) {
	if ownerDead(w.owner) {
		idleUpdateOnce(w.tr, w.cwd, w.session, w.stderr) // no SessionEnd on a crash; the stamp makes it a no-op if one ran
		return true
	}
	fi, err := os.Stat(w.tr)
	if err != nil {
		return os.IsNotExist(err) // transcript gone ends the watcher; any other error retries next poll
	}
	if time.Since(fi.ModTime()) < w.idle {
		return false // main transcript alone is still active: skip the subagent globs entirely
	}
	latest := newestActivity(w.tr, fi.ModTime())
	if time.Since(latest) < w.idle || !latest.After(w.seen) {
		return false // a subagent is still active, or already evaluated at this mtime
	}
	w.dispatch(latest)
	return false
}

// dispatch runs the idle update for the activity at latest and notes it; a failed spawn is retried by the next poll.
func (w *watchLoop) dispatch(latest time.Time) {
	dispatched, spawnFailed := idleUpdateOnce(w.tr, w.cwd, w.session, w.stderr)
	if !spawnFailed {
		w.seen = latest
	}
	if dispatched {
		fmt.Fprintf(w.stderr, "memory watch: %s quiet for %s — final update dispatched\n", w.session, w.idle.String())
	}
}

// newestActivity is the newest mtime across the main and its subagent transcripts: a busy subagent keeps it live.
func newestActivity(tr string, mainMtime time.Time) time.Time {
	latest := mainMtime
	dir := inflight.SubagentDir(tr)
	if dir == "" {
		return latest
	}
	for _, m := range globIn(dir, "agent-*.jsonl") {
		if fi, err := os.Stat(m); err == nil && fi.ModTime().After(latest) {
			latest = fi.ModTime()
		}
	}
	return latest
}

// idleUpdateOnce dispatches the final update for uncovered work; spawnFailed = a due worker did not start.
func idleUpdateOnce(tr, cwd, session string, note io.Writer) (dispatched, spawnFailed bool) {
	cwd = SessionRoot(session, cwd)
	withStampLock(session, func() { // same session lock as UpdateIfNew
		covered := 0
		if st, ok := readStamp(stampPath(session)); ok {
			covered = st.edits
		}
		edits := CountEdits(session, tr)
		if edits < updateMinEdits || edits <= covered {
			return
		}
		if !DispatchUpdate(UpdateRequest{SessionID: session, Transcript: tr, CWD: cwd}, "idle", edits, time.Now()) {
			fmt.Fprintf(note, "memory watch: worker not spawned for %s (%d uncovered edits)\n", session, edits-covered)
			spawnFailed = true
			return
		}
		dispatched = true
	})
	return dispatched, spawnFailed
}

// ownerDead trusts the probe only where it works: off unix Alive is always false, which would disable the watcher.
func ownerDead(pid int) bool {
	return pid > 0 && proc.Alive(os.Getpid()) && !proc.Alive(pid)
}
