// The idle watcher is the SessionEnd-INDEPENDENT final update: Claude Code fires Stop every turn but
// SessionEnd only on a clean exit — a crash, a kill, or a closed terminal never fires it, and the last
// stretch of work stays in the transcript. One detached watcher per session polls the transcript's
// mtime; once it has been quiet for the idle period and holds edits no dispatch covered, it dispatches
// the same detached worker the Stop gate uses, then keeps watching until its owner or lifetime is gone.
package memhook

import (
	"fmt"
	"github.com/edotau/claude-code/internal/memory"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"

	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
)

// UpdateIdleAfter: a transcript this quiet is an ended session. Poll and lifetime bound the watcher.
const (
	UpdateIdleAfter = 10 * time.Minute
	updateWatchLife = 24 * time.Hour
)

// updateWatchPoll is the mtime poll gap; a var so tests can tighten it.
var updateWatchPoll = time.Minute

// spawnWatcher is the detached-spawn seam over `claude-code memory watch`; tests replace it.
var spawnWatcher = func(args []string, cwd string) bool {
	self, err := os.Executable()
	if err != nil {
		return false
	}
	logPath := filepath.Join(memory.StateDir(), "watch-"+filepath.Base(args[1])+".log")
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return false
	}
	defer log.Close()
	return proc.SpawnDetached(self, append([]string{"memory", "watch"}, args...), cwd, log)
}

// spawnIdleWatcher starts THIS session's watcher once: a claim file makes every later Stop a no-op.
func spawnIdleWatcher(in *core.Input) {
	if in.SessionID == "" {
		return
	}
	claim := filepath.Join(memory.StateDir(), "watch-"+filepath.Base(in.SessionID))
	f, err := os.OpenFile(claim, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return // claimed by an earlier Stop (or the state dir is unwritable — the Stop gate still runs)
	}
	_ = f.Close()
	args := []string{"--session", in.SessionID, "--transcript", in.TranscriptPath, "--cwd", in.CWD,
		"--owner", strconv.Itoa(inflight.SessionOwner())}
	if !spawnWatcher(args, in.CWD) {
		_ = os.Remove(claim) // let the next Stop try again
	}
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
		return true // session process gone: SessionEnd (or the last Stop) already had its chance
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
	dispatched, spawnFailed := idleUpdateOnce(w.tr, w.cwd, w.session, w.stderr)
	if !spawnFailed {
		w.seen = latest // a failed spawn leaves seen behind so the next poll retries
	}
	if dispatched {
		fmt.Fprintf(w.stderr, "memory watch: %s quiet for %s — final update dispatched\n", w.session, w.idle.String())
	}
	return false
}

// newestActivity is the newest mtime across the main transcript and this session's subagent transcripts
// (<session>/subagents/agent-*.jsonl) — a subagent still appending after the parent goes idle must not
// let the watcher treat the session as quiet.
func newestActivity(tr string, mainMtime time.Time) time.Time {
	latest := mainMtime
	dir := inflight.SubagentDir(tr)
	if dir == "" {
		return latest
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "agent-*.jsonl"))
	for _, m := range matches {
		if fi, err := os.Stat(m); err == nil && fi.ModTime().After(latest) {
			latest = fi.ModTime()
		}
	}
	return latest
}

// idleUpdateOnce dispatches the final update when the transcript holds substantive work no dispatch
// covered; the stamp then carries the count so the next idle check (and any late Stop) stays quiet.
// spawnFailed reports a worker that should have started but did not.
func idleUpdateOnce(tr, cwd, session string, note io.Writer) (dispatched, spawnFailed bool) {
	cwd = memory.SessionRoot(session, cwd)
	ran := false
	err := paths.WithFileLock(stampPath(session), func() error { // same session lock as updateIfNew
		ran = true
		dispatched, spawnFailed = idleUpdateLocked(tr, cwd, session, note)
		return nil
	})
	if err != nil && !ran {
		return idleUpdateLocked(tr, cwd, session, note) // no lock available: unserialized beats skipping the update
	}
	return dispatched, spawnFailed
}

// idleUpdateLocked is idleUpdateOnce's read-dispatch-write body; the caller holds the session stamp lock.
func idleUpdateLocked(tr, cwd, session string, note io.Writer) (dispatched, spawnFailed bool) {
	stamp := stampPath(session)
	covered := 0
	if st, ok := readStamp(stamp); ok {
		covered = st.edits
	}
	edits := countEdits(tr, countPath(session))
	if edits < updateMinEdits || edits <= covered {
		return false, false
	}
	if !spawnUpdate(tr, cwd, session) {
		fmt.Fprintf(note, "memory watch: worker not spawned for %s (%d uncovered edits)\n", session, edits-covered)
		return false, true
	}
	_ = writeStamp(stamp, time.Now(), "bg", edits) // best effort: a lost stamp only re-dispatches later
	return true, false
}

// ownerDead is true only when the probe itself is trusted: off unix Alive is always false, and
// treating every pid as dead there would silently disable the idle-update watcher.
func ownerDead(pid int) bool {
	return pid > 0 && proc.Alive(os.Getpid()) && !proc.Alive(pid)
}
