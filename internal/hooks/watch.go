// The idle watcher is the SessionEnd-INDEPENDENT final harvest: Claude Code fires Stop every turn but
// SessionEnd only on a clean exit — a crash, a kill, or a closed terminal never fires it, and the last
// stretch of work stays in the transcript. One detached watcher per session polls the transcript's
// mtime; once it has been quiet for the idle period and holds edits no dispatch covered, it dispatches
// the same detached worker the Stop gate uses, then keeps watching until its owner or lifetime is gone.
package hooks

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/proc"
)

// harvestIdleAfter: a transcript this quiet is an ended session. Poll and lifetime bound the watcher.
const (
	harvestIdleAfter = 10 * time.Minute
	harvestWatchLife = 24 * time.Hour
)

// harvestWatchPoll is the mtime poll gap; a var so tests can tighten it.
var harvestWatchPoll = time.Minute

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
func spawnIdleWatcher(in *Input) {
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

// IdleHarvestWatch is `claude-code memory watch`: the detached per-session watcher described above.
func IdleHarvestWatch(args []string, stderr io.Writer) int {
	fs := helptext.NewFlagSet("memory watch", helptext.Page{Usage: "claude-code memory watch [flags]",
		About: "Detached per-session watcher the Stop gate spawns; harvests the bank once the session goes idle."})
	fs.SetOutput(stderr)
	session := fs.String("session", "", "session id (names the stamp)")
	tr := fs.String("transcript", "", "the session's transcript path")
	cwd := fs.String("cwd", "", "project root whose bank to harvest")
	idle := fs.Duration("idle", harvestIdleAfter, "quiet period before the final harvest")
	owner := fs.Int("owner", 0, "session process pid; the watcher exits once it is gone (0 = unbounded)")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if *session == "" || *tr == "" || *idle <= 0 {
		fmt.Fprintln(stderr, "memory watch: --session, --transcript and a positive --idle are required")
		return 2
	}
	if fi, err := os.Stat(*cwd); err != nil || !fi.IsDir() {
		fmt.Fprintf(stderr, "memory watch: --cwd %q is not a directory\n", *cwd)
		return 2
	}
	deadline := time.Now().Add(harvestWatchLife)
	var seen time.Time // mtime last evaluated: the transcript is append-only, so unchanged ⇒ same edit count
	for time.Now().Before(deadline) {
		time.Sleep(harvestWatchPoll)
		if ownerDead(*owner) {
			return 0 // session process gone: SessionEnd (or the last Stop) already had its chance
		}
		fi, err := os.Stat(*tr)
		if err != nil {
			return 0 // transcript gone: nothing left to harvest
		}
		if time.Since(fi.ModTime()) < *idle {
			continue // main transcript alone is still active: skip the subagent globs entirely
		}
		latest := newestActivity(*tr, fi.ModTime())
		if time.Since(latest) < *idle || !latest.After(seen) {
			continue // a subagent is still active, or already evaluated at this mtime
		}
		seen = latest
		if idleHarvestOnce(*tr, *cwd, *session, stderr) {
			fmt.Fprintf(stderr, "memory watch: %s quiet for %s — final harvest dispatched\n", *session, idle.String())
		}
	}
	return 0
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

// idleHarvestOnce dispatches the final harvest when the transcript holds substantive work no dispatch
// covered; the stamp then carries the count so the next idle check (and any late Stop) stays quiet.
func idleHarvestOnce(tr, cwd, session string, note io.Writer) bool {
	stamp := stampPath(session)
	covered := 0
	if st, ok := readStamp(stamp); ok {
		covered = st.edits
	}
	edits := countEdits(tr, countPath(session))
	if edits < harvestMinEdits || edits <= covered {
		return false
	}
	if !spawnHarvest(tr, cwd, session) {
		fmt.Fprintf(note, "memory watch: worker not spawned for %s (%d uncovered edits)\n", session, edits-covered)
		return false
	}
	writeStamp(stamp, time.Now(), "bg", edits)
	return true
}
