package memory

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/dev/transcript"
	"github.com/edotau/claude-code/internal/platform/parallel"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/proc"
	"github.com/edotau/claude-code/internal/routing/inflight"
	"github.com/edotau/claude-code/internal/routing/models"
)

// Update gate in work units (edits + turns/2); a re-arm needs 10 new units AND 15 min: chatty re-arms evict history.
const (
	updateMinEdits      = transcript.CommitEditWeight // keeps a `git commit` worth exactly one gate floor
	updateRearmEdits    = 10
	updateRearmCooldown = 15 * time.Minute
	updateMaxBlocks     = 3
)

// Update triggers: the hook that asked for the update.
const (
	TriggerStop       = "stop"
	TriggerPreCompact = "pre-compact"
	TriggerSessionEnd = "session-end"
)

// UpdateRequest is one session's update ask; OnDispatch (optional) runs after each successful worker spawn.
type UpdateRequest struct {
	SessionID, Transcript, CWD string
	OnDispatch                 func(trigger string, work int, now time.Time)
}

// gateResult is the gate's verdict: Blocked = Stop gets a not-saved note (work units uncovered); FailedOpen = unsaved.
type gateResult struct {
	Blocked    bool
	FailedOpen bool
}

// UpdateIfNew spawns an update when work (edits or user turns) is new since the stamp.
func UpdateIfNew(r UpdateRequest, trigger string, now time.Time) gateResult {
	var res gateResult
	withStampLock(r.SessionID, func() { // hooks of one session are separate processes
		res.Blocked, res.FailedOpen = updateIfNewLocked(r, trigger, now)
	})
	return res
}

// stampLockWait caps the stamp-lock wait below the shortest memory hook budget (memory-end, 15 s).
var stampLockWait = 5 * time.Second

// withStampLock runs fn under the session's stamp lock; with no lock, unserialized beats skipping.
func withStampLock(sessionID string, fn func()) {
	ran := false
	err := paths.WithFileLockWithin(stampPath(sessionID), stampLockWait, func() error {
		ran = true
		fn()
		return nil
	})
	if err != nil && !ran {
		fn()
	}
}

func updateIfNewLocked(r UpdateRequest, trigger string, now time.Time) (blocked, failedOpen bool) {
	stamp := stampPath(r.SessionID)
	st, ok := readStamp(stamp)
	covered := 0
	if ok {
		covered = st.edits
	}
	if trigger == TriggerStop && ok {
		return stampedStop(r, st, stamp, now)
	}
	floor := updateMinEdits
	if trigger == TriggerPreCompact {
		floor = 1
	}
	work := countSessionEdits(r.SessionID, r.Transcript)
	if work > covered && work >= floor {
		if dispatchUpdate(r, trigger, work, now) {
			return false, false
		}
		if trigger == TriggerStop {
			// Stamp the old covered count: this work was never dispatched, so SessionEnd/PreCompact must still see it.
			if writeStamp(stamp, now, "1", covered) != nil {
				return false, true // fail open: the block count cannot persist
			}
			return true, false
		}
	}
	return false, false
}

// stampedStop is the Stop gate for a session that already has a stamp: re-arm, release, retry, or block up to the cap.
func stampedStop(r UpdateRequest, st updateStamp, stamp string, now time.Time) (blocked, failedOpen bool) {
	if st.state == "bg" || st.state == "catchup" {
		if now.Sub(st.at) < updateRearmCooldown {
			return false, false // cheap gate before the whole-transcript count
		}
		work := countSessionEdits(r.SessionID, r.Transcript)
		if work >= st.edits+updateRearmEdits {
			dispatchUpdate(r, TriggerStop, work, now)
		}
		return false, false
	}
	if newestMtime().After(st.at) {
		_ = writeStamp(stamp, now, "bg", st.edits) // ran by hand: released, re-arms; a lost stamp re-dispatches
		return false, false
	}
	if st.blocks >= updateMaxBlocks {
		if st.blocks == updateMaxBlocks { // say so once, at the cap; later Stops stay quiet
			persisted := writeStamp(stamp, st.at, strconv.Itoa(st.blocks+1), st.edits) == nil
			return false, persisted // the notice shows once, only when its stamp persisted
		}
		return false, false // fail open
	}
	if retryFailedStop(r, st, now) {
		return false, false
	}
	if writeStamp(stamp, st.at, strconv.Itoa(st.blocks+1), st.edits) != nil {
		return false, true // fail open: the block count cannot persist
	}
	return true, false
}

// retryFailedStop re-spawns a failed Stop's uncovered work on each blocked Stop; success writes the "bg" stamp.
func retryFailedStop(r UpdateRequest, st updateStamp, now time.Time) bool {
	work := countSessionEdits(r.SessionID, r.Transcript)
	return work > st.edits && work >= updateMinEdits && dispatchUpdate(r, TriggerStop, work, now)
}

// Updatable reports whether a session can be updated: ids present, not an update child or dispatched leaf, a file.
func Updatable(sessionID, transcriptPath string) bool {
	if sessionID == "" || transcriptPath == "" || IsUpdateChild() || os.Getenv(proc.DispatchIDEnv) != "" {
		return false
	}
	fi, err := os.Stat(transcriptPath)
	return err == nil && !fi.IsDir()
}

// updateStamp is "<RFC3339>|<bg|block count|catchup>|<edits covered>[|<catch-up runs started>]".
type updateStamp struct {
	at       time.Time
	blocks   int
	edits    int
	state    string // raw second field: "bg", "catchup", or a block count
	attempts int    // optional fourth field; 0 when absent
}

func stampPath(sessionID string) string {
	return filepath.Join(paths.MemoryStateDir(), "stamp-"+filepath.Base(sessionID))
}

func readStamp(path string) (updateStamp, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return updateStamp{}, false
	}
	parts := strings.Split(strings.TrimSpace(string(b)), "|")
	if len(parts) != 3 && len(parts) != 4 {
		return updateStamp{}, false
	}
	at, err := time.Parse(time.RFC3339, parts[0])
	if err != nil {
		return updateStamp{}, false
	}
	st := updateStamp{at: at, state: parts[1]}
	// Only a numeric parts[1] is a block count; "bg" or "catchup" leaves st.blocks at zero.
	if n, err := strconv.Atoi(parts[1]); err == nil {
		st.blocks = n
	}
	st.edits, _ = strconv.Atoi(parts[2])
	if len(parts) == 4 {
		st.attempts, _ = strconv.Atoi(parts[3])
	}
	return st, true
}

func writeStamp(path string, at time.Time, state string, edits int) error {
	return writeStampAttempts(path, at, state, edits, 0)
}

// writeStampAttempts is writeStamp plus the catch-up attempt count, written only when above zero.
func writeStampAttempts(path string, at time.Time, state string, edits, attempts int) error {
	line := at.Format(time.RFC3339) + "|" + state + "|" + strconv.Itoa(edits)
	if attempts > 0 {
		line += "|" + strconv.Itoa(attempts)
	}
	err := paths.AtomicWrite(path, []byte(line), 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "session-update: write stamp %s: %v\n", path, err)
	}
	return err
}

// dispatchUpdate spawns the worker and records it; a Stop's spawn also gets a watcher for the session's final state.
func dispatchUpdate(r UpdateRequest, trigger string, work int, now time.Time) bool {
	if !spawn.update(r.Transcript, r.CWD, r.SessionID) {
		return false
	}
	if r.OnDispatch != nil {
		r.OnDispatch(trigger, work, now)
	}
	_ = writeStamp(stampPath(r.SessionID), now, "bg", work) // best effort: a lost stamp only re-dispatches later
	if trigger == TriggerStop {
		spawnIdleWatcher(r.SessionID, r.Transcript, r.CWD)
	}
	return true
}

const userTurnsPerEdit = 2 // 2 user turns weigh as 1 edit; 6 turns equal updateMinEdits

// sidecarLockWait bounds how long subagentEdits waits for the sidecar lock; a var so tests can shorten it.
var sidecarLockWait = 2 * time.Second

// countState is the transcript count cache; subEdits/subBytes cache subagents' edits against their summed file size.
type countState struct {
	edits    int
	offset   int64
	subEdits int
	subBytes int64
	turns    int
}

func countPath(sessionID string) string {
	return filepath.Join(paths.MemoryStateDir(), "count-"+filepath.Base(sessionID))
}

// readCountState decodes the four-field count line: edits|offset|subEdits:subBytes|turns.
func readCountState(path string) (countState, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return countState{}, false
	}
	parts := strings.Split(strings.TrimSpace(string(b)), "|")
	if len(parts) != 4 {
		return countState{}, false
	}
	edits, err1 := strconv.Atoi(parts[0])
	offset, err2 := strconv.ParseInt(parts[1], 10, 64)
	subEditsStr, subBytesStr, ok := strings.Cut(parts[2], ":")
	subEdits, err3 := strconv.Atoi(subEditsStr)
	subBytes, err4 := strconv.ParseInt(subBytesStr, 10, 64)
	turns, err5 := strconv.Atoi(parts[3])
	if !ok || errors.Join(err1, err2, err3, err4, err5) != nil || edits < 0 || offset < 0 || subEdits < 0 || subBytes < 0 || turns < 0 {
		return countState{}, false
	}
	return countState{edits: edits, offset: offset, subEdits: subEdits, subBytes: subBytes, turns: turns}, true
}

func writeCountState(path string, cs countState) {
	line := strconv.Itoa(cs.edits) + "|" + strconv.FormatInt(cs.offset, 10) + "|" +
		strconv.Itoa(cs.subEdits) + ":" + strconv.FormatInt(cs.subBytes, 10) + "|" +
		strconv.Itoa(cs.turns)
	if err := paths.AtomicWrite(path, []byte(line), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "session-update: write count state %s: %v\n", path, err)
	}
}

// countSessionEdits is countEdits against the session's own count file.
func countSessionEdits(sessionID, transcriptPath string) int {
	return countEdits(transcriptPath, countPath(sessionID))
}

// countEdits returns work units (edits + turns/2) from the last complete line; subagent files rescan on size moves.
func countEdits(transcriptPath, countFile string) int {
	return countWork(transcriptPath, countFile, true)
}

// readOnlyEdits is countEdits without cache writes, for --dry-run.
func readOnlyEdits(trPath, countFile string) int { return countWork(trPath, countFile, false) }

// countWork is countEdits; write=false counts the same units but persists no cache (the --dry-run view).
func countWork(transcriptPath, countFile string, write bool) int {
	fi, err := os.Stat(transcriptPath)
	if err != nil || fi.IsDir() {
		return 0
	}
	cs, _ := readCountState(countFile)
	if fi.Size() < cs.offset {
		cs = resetCountState(countFile, write)
	}
	mainProgressed := false
	if fi.Size() != cs.offset {
		cs, mainProgressed = advanceMainCount(cs, transcriptPath, countFile, write)
	}
	subEdits, subBytes := subagentEdits(transcriptPath, cs.subBytes, cs.subEdits, write)
	if write && (mainProgressed || subEdits != cs.subEdits || subBytes != cs.subBytes) {
		cs.subEdits, cs.subBytes = subEdits, subBytes
		writeCountState(countFile, cs)
	}
	return cs.edits + subEdits + cs.turns/userTurnsPerEdit
}

// resetCountState drops the cache (the transcript shrank or rotated) and returns a zero state.
func resetCountState(countFile string, write bool) countState {
	if write {
		_ = os.Remove(countFile)
	}
	return countState{}
}

// advanceMainCount folds the transcript bytes appended since cs.offset into cs and reports whether the offset moved.
func advanceMainCount(cs countState, transcriptPath, countFile string, write bool) (countState, bool) {
	deltaEdits, deltaTurns, next := transcript.CountWorkFrom(transcriptPath, cs.offset)
	if next < cs.offset {
		cs = resetCountState(countFile, write)
	}
	cs.edits += deltaEdits
	cs.turns += deltaTurns
	moved := next != cs.offset
	if moved {
		cs.offset = next
	}
	return cs, moved
}

// subFileState is one subagent file's counted offset and edits, cached in the sidecar so a bump rescans only new bytes.
type subFileState struct {
	Offset int64 `json:"offset"`
	Edits  int   `json:"edits"`
}

func sidecarPath(subDir string) string { return filepath.Join(subDir, ".offsets.json") }

func readSidecar(path string) map[string]subFileState {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var m map[string]subFileState
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	return m
}

func writeSidecar(path string, m map[string]subFileState) {
	if b, err := json.Marshal(m); err == nil {
		_ = paths.AtomicWrite(path, b, 0o600)
	}
}

// agentTranscripts lists a subagents dir's leaf and Workflow-run (workflows/<run>/) agent transcripts.
func agentTranscripts(dir string) []string {
	return append(transcript.GlobIn(dir, "agent-*.jsonl"), transcript.GlobIn(dir, filepath.Join("workflows", "*", "agent-*.jsonl"))...)
}

// subagentEdits recounts agent-*.jsonl edits past cached offsets when their summed size moved; write=false: no lock.
func subagentEdits(transcriptPath string, cachedBytes int64, cachedEdits int, write bool) (edits int, totalBytes int64) {
	dir := inflight.SubagentDir(transcriptPath)
	if dir == "" {
		return 0, 0
	}
	matches := agentTranscripts(dir)
	sizes := make(map[string]int64, len(matches))
	for _, m := range matches {
		if fi, err := os.Stat(m); err == nil {
			sizes[m] = fi.Size()
			totalBytes += fi.Size()
		}
	}
	if totalBytes == cachedBytes {
		return cachedEdits, totalBytes
	}
	sidecar := sidecarPath(dir)
	if !write {
		return recountSubagents(matches, sizes, sidecar, false), totalBytes
	}
	// Stop and SessionEnd race on one session: serialize so neither regresses the other's offsets; a timeout skips.
	ran, lockErr := paths.TryFileLock(sidecar+".lock", sidecarLockWait, func() error {
		edits = recountSubagents(matches, sizes, sidecar, true)
		return nil
	})
	if ran {
		return edits, totalBytes
	}
	if lockErr != nil {
		// Lock file unopenable (read-only subagents dir): compute unlocked and skip the write, never freeze the count.
		return recountSubagents(matches, sizes, sidecar, false), totalBytes
	}
	// Contention timeout: keep both cached totals; new bytes against stale edits would skip the recount forever.
	return cachedEdits, cachedBytes
}

const recountWorkers = 8

// recountSubagents sums edits past each cached offset; write=false never persists a stale sidecar (unlocked path).
func recountSubagents(matches []string, sizes map[string]int64, sidecar string, write bool) int {
	prevState := readSidecar(sidecar)
	states, err := parallel.Map(context.Background(), matches, recountWorkers, func(_ context.Context, m string) (subFileState, error) {
		st := prevState[m] // nil map read: zero value, no rescan skipped
		if sizes[m] < st.Offset {
			st = subFileState{} // shrank or rotated: rescan from 0
		}
		if sizes[m] != st.Offset {
			delta, _, off := transcript.CountWorkFrom(m, st.Offset)
			st.Edits += delta
			st.Offset = off
		}
		return st, nil
	})
	parallel.Rethrow(err)
	next := make(map[string]subFileState, len(matches))
	sum := 0
	for i, m := range matches {
		sum += states[i].Edits
		next[m] = states[i]
	}
	if write {
		writeSidecar(sidecar, next)
	}
	return sum
}

// StoreCopy is a throwaway git clone of the store for the fallback to read; a var so tests need no repo.
var StoreCopy = func(ctx context.Context) (string, func(), error) {
	dir, err := os.MkdirTemp("", "memory-fallback-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	if out, cerr := exec.CommandContext(ctx, "git", "clone", "--quiet", "--local", paths.MemoryStoreDir(), dir).CombinedOutput(); cerr != nil {
		cleanup()
		return "", nil, fmt.Errorf("git clone of the store: %v: %s", cerr, strings.TrimSpace(string(out)))
	}
	return dir, cleanup, nil
}

// CodeModeRun is the one code-mode Pi dispatch behind the typed-write fallback; ask registers it.
var CodeModeRun func(ctx context.Context, prompt, workdir, schema, model string) (answer, who string, err error)

// CodeFallback allows one CodeModeRun per job: a second call fails without dispatching.
type CodeFallback struct {
	Model string // "" is opus
	used  atomic.Bool
	last  atomic.Pointer[string] // how the one dispatch ended
}

type fallbackKey struct{}

// WithCodeFallback carries the job's one fallback so the runner and the typed writer share it.
func WithCodeFallback(ctx context.Context, f *CodeFallback) context.Context {
	return context.WithValue(ctx, fallbackKey{}, f)
}

// JobCodeFallback is the fallback WithCodeFallback set, or nil.
func JobCodeFallback(ctx context.Context) *CodeFallback {
	f, _ := ctx.Value(fallbackKey{}).(*CodeFallback)
	return f
}

// FallbackModel maps the fallback's model name to a catalog id or alias; an unknown id is an error.
func FallbackModel(name string) (string, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		name = models.Opus
	}
	if !models.IsKnownSpelling(name) {
		return "", fmt.Errorf("model %q is not in the catalog", name)
	}
	return name, nil
}

// outcome is how the one dispatch ended, or that it is still running.
func (f *CodeFallback) outcome() string {
	if p := f.last.Load(); p != nil {
		return *p
	}
	return "still running"
}

// Used reports whether the job's one dispatch was taken.
func (f *CodeFallback) Used() bool { return f.used.Load() }

// Run asks the code-mode agent, over a git copy of the store, for a typed Batch; the workdir argument is ignored.
func (f *CodeFallback) Run(ctx context.Context, prompt, workdir, schema string) (answer, who string, err error) {
	if CodeModeRun == nil {
		return "", "", errors.New("code-mode fallback unavailable: this process registered no agent (a hook never dispatches one)")
	}
	if !f.used.CompareAndSwap(false, true) {
		return "", "", fmt.Errorf("code-mode fallback already used for this job (%s)", f.outcome())
	}
	dir, cleanup, err := StoreCopy(ctx)
	if err != nil {
		return "", "", err
	}
	defer cleanup()
	prompt = strings.ReplaceAll(prompt, paths.MemoryStoreDir(), dir) + "\n\nWrite nothing and run no memory command: answer with the typed batch only."
	answer, who, err = CodeModeRun(ctx, prompt, dir, schema, f.Model)
	if err == nil {
		if _, derr := decodeBatch(answer); derr != nil {
			err = fmt.Errorf("code-mode answer rejected: %w", derr)
		}
	}
	out := who + " answered"
	if err != nil {
		out, answer = who+" failed: "+err.Error(), ""
	}
	f.last.Store(&out)
	return answer, who, err
}

// Retry gives the one fallback a failed first answer and applies the corrected Batch; both failures are named.
func (f *CodeFallback) Retry(ctx context.Context, prompt, workdir, schema, first string, failed error, apply func(answer string) error) error {
	prompt += assets.MemoryRetryNote(failed.Error())
	answer, who, err := f.Run(ctx, prompt, workdir, schema)
	if err == nil {
		if err = apply(answer); err == nil {
			return nil
		}
	}
	return fmt.Errorf("typed write failed on %s (%v); code-mode fallback on %s failed: %w", cmp.Or(first, "the first model"), failed, cmp.Or(who, f.Model, "opus"), err)
}

// ValidateBatchAnswer reports why a typed answer is unusable; nil means it decodes and validates.
func ValidateBatchAnswer(answer string) error {
	_, err := decodeBatch(answer)
	return err
}

// UpdateIdleAfter: a transcript this quiet is an ended session. Poll and lifetime bound the watcher.
const (
	UpdateIdleAfter = 10 * time.Minute
	updateWatchLife = 24 * time.Hour
)

// updateWatchPoll is the mtime poll gap; a var so tests can tighten it.
var updateWatchPoll = time.Minute

func watchClaimPath(sessionID string) string {
	return filepath.Join(paths.MemoryStateDir(), "watch-"+filepath.Base(sessionID))
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
		if !retakeDeadClaim(claim) {
			return false
		}
	}
	return false
}

// retakeDeadClaim removes a dead watcher's claim under a lock, re-checking it so two Stops never delete each other's.
func retakeDeadClaim(claim string) bool {
	retaken := false
	ran, err := paths.TryFileLock(claim+".lock", 0, func() error {
		if watcherDead(claim) {
			retaken = os.Remove(claim) == nil
		}
		return nil
	})
	return ran && err == nil && retaken
}

// claimGrace is how long an empty claim counts as a watcher still starting before it is a dead leftover.
const claimGrace = time.Minute

// watcherDead reports whether the claim names a pid that is gone, or is empty for longer than claimGrace.
func watcherDead(claim string) bool {
	b, err := os.ReadFile(claim)
	if err != nil {
		return false
	}
	pid, perr := strconv.Atoi(strings.TrimSpace(string(b)))
	if perr != nil {
		fi, serr := os.Stat(claim)
		return len(b) == 0 && serr == nil && time.Since(fi.ModTime()) > claimGrace
	}
	return ownerDead(pid)
}

// releaseEmptyClaim removes the empty claim spawnIdleWatcher made for a watcher that exits before writing its pid.
func releaseEmptyClaim(session string) {
	if session == "" {
		return
	}
	if fi, err := os.Stat(watchClaimPath(session)); err == nil && fi.Size() == 0 {
		_ = os.Remove(watchClaimPath(session))
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
		releaseEmptyClaim(o.Session)
		return 2
	}
	if fi, err := os.Stat(o.CWD); err != nil || !fi.IsDir() {
		fmt.Fprintf(stderr, "memory watch: --cwd %q is not a directory\n", o.CWD)
		releaseEmptyClaim(o.Session)
		return 2
	}
	// The claim holds this pid so a later Stop retakes it after a kill; the defer frees it for a fresh watcher.
	if err := os.WriteFile(watchClaimPath(o.Session), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		fmt.Fprintf(stderr, "memory watch: write claim: %v\n", err)
		_ = os.Remove(watchClaimPath(o.Session)) // an empty claim would never be retaken; the stamp dedupes a rival
	}
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
	for _, m := range agentTranscripts(dir) {
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
		edits := countSessionEdits(session, tr)
		if edits < updateMinEdits || edits <= covered {
			return
		}
		if !dispatchUpdate(UpdateRequest{SessionID: session, Transcript: tr, CWD: cwd}, "idle", edits, time.Now()) {
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
