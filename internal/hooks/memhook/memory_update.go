package memhook

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
	memrecall "github.com/edotau/claude-code/internal/memory/recall"

	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/memory/bank"
	"github.com/edotau/claude-code/internal/memory/recall/embed"
	"github.com/edotau/claude-code/internal/memstore"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/transcript"
	"github.com/edotau/claude-code/internal/worktree"
)

// Update gate thresholds (work units: edits + user turns/2): the floor asks "was this substantive work"; re-arming needs 10 NEW units AND 15 min, since every update appends a sessionHistory entry and a chatty re-arm evicts others.
const (
	updateMinEdits      = transcript.CommitEditWeight // keeps a `git commit` worth exactly one gate floor
	updateRearmEdits    = 10
	updateRearmCooldown = 15 * time.Minute
	updateMaxBlocks     = 3
	userTurnsPerEdit    = 2 // 2 user turns weigh as 1 edit; 6 turns equal updateMinEdits
)

const (
	triggerStop       = "stop"
	triggerPreCompact = "pre-compact"
	triggerSessionEnd = "session-end"
)

// updateNotSaved is the visible line when the Stop gate fails open without an update.
const updateNotSaved = "memory update NOT saved; run `claude-code memory update`"

// sidecarLockWait bounds how long subagentEdits waits for the sidecar lock; a var so tests can shorten it.
var sidecarLockWait = 2 * time.Second

// sessionUpdate (Stop) dispatches a detached `memory update` once a session has substantive work; with no spawn possible it blocks at most updateMaxBlocks times asking for `claude-code memory update`.
func sessionUpdate(in *core.Input, stderr io.Writer) (code int, failedOpen bool) {
	in = AgentInput(sessionRootInput(in))
	refreshEmbeddings(in.CWD)
	if !updatable(in) {
		return core.ExitProceed, false
	}
	blocked, work, failedOpen := updateIfNew(in, triggerStop, time.Now())
	if blocked {
		return blockForUpdate(stderr, work), false
	}
	return core.ExitProceed, failedOpen
}

// End (SessionEnd) updates the final state when work since the last dispatch crosses the floor.
func End(in *core.Input) core.Result {
	in = AgentInput(sessionRootInput(in))
	refreshEmbeddings(in.CWD)
	if !updatable(in) {
		return core.Result{}
	}
	updateIfNew(in, triggerSessionEnd, time.Now())
	return core.Result{}
}

// Compact (ADK add_events_to_memory): before Claude Code compacts the context, update any work no earlier dispatch covered so the bank holds them; the transcript file itself survives compaction, so this never blocks.
func Compact(in *core.Input) core.Result {
	in = AgentInput(sessionRootInput(in))
	if !updatable(in) {
		return core.Result{}
	}
	updateIfNew(in, triggerPreCompact, time.Now())
	return core.Result{}
}

// updateIfNew spawns an update when work (edits or user turns) is new since the stamp; blocked = Stop must block.
func updateIfNew(in *core.Input, trigger string, now time.Time) (blocked bool, work int, failedOpen bool) {
	ran := false
	err := paths.WithFileLock(stampPath(in.SessionID), func() error { // hooks of one session are separate processes
		ran = true
		blocked, work, failedOpen = updateIfNewLocked(in, trigger, now)
		return nil
	})
	if err != nil && !ran {
		return updateIfNewLocked(in, trigger, now) // no lock available: unserialized beats skipping the update
	}
	return blocked, work, failedOpen
}

func updateIfNewLocked(in *core.Input, trigger string, now time.Time) (blocked bool, work int, failedOpen bool) {
	stamp := stampPath(in.SessionID)
	st, ok := readStamp(stamp)
	covered := 0
	if ok {
		covered = st.edits
	}
	if trigger == triggerStop && ok {
		return stampedStop(in, st, stamp, now)
	}
	floor := updateMinEdits
	if trigger == triggerPreCompact {
		floor = 1
	}
	work = countEdits(in.TranscriptPath, countPath(in.SessionID))
	if work > covered && work >= floor {
		if dispatchUpdate(in, trigger, work, now) {
			return false, 0, false
		}
		if trigger == triggerStop {
			if writeStamp(stamp, now, "1", work) != nil {
				return false, 0, true // fail open: the block count cannot persist
			}
			return true, work, false
		}
	}
	return false, 0, false
}

// stampedStop is the Stop gate for a session that already has a stamp: re-arm, release, or block up to the cap.
func stampedStop(in *core.Input, st updateStamp, stamp string, now time.Time) (blocked bool, work int, failedOpen bool) {
	if st.bg || st.state == "catchup" {
		if now.Sub(st.at) < updateRearmCooldown {
			return false, 0, false // cheap gate before the whole-transcript count
		}
		work = countEdits(in.TranscriptPath, countPath(in.SessionID))
		if work >= st.edits+updateRearmEdits {
			dispatchUpdate(in, triggerStop, work, now)
		}
		return false, 0, false
	}
	if bank.NewestMtime(memstore.Dir(in.CWD)).After(st.at) {
		_ = writeStamp(stamp, now, "bg", st.edits) // ran by hand: released, re-arms; a lost stamp re-dispatches
		return false, 0, false
	}
	if st.blocks >= updateMaxBlocks {
		if st.blocks == updateMaxBlocks { // say so once, at the cap; later Stops stay quiet
			persisted := writeStamp(stamp, st.at, strconv.Itoa(st.blocks+1), st.edits) == nil
			return false, 0, persisted // the notice shows once, only when its stamp persisted
		}
		return false, 0, false // fail open
	}
	if writeStamp(stamp, st.at, strconv.Itoa(st.blocks+1), st.edits) != nil {
		return false, 0, true // fail open: the block count cannot persist
	}
	return true, st.edits, false
}

// logUpdateToLedger notes an update in the session's linked-worktree ledger; best effort, never fatal.
func logUpdateToLedger(in *core.Input, trigger string, work int, now time.Time) {
	_, _ = worktree.AppendLedger(in.CWD, fmt.Sprintf("update (%s): %d edits, session %s", trigger, work, in.SessionID), now)
}

func updatable(in *core.Input) bool {
	if in.SessionID == "" || in.TranscriptPath == "" || bank.IsUpdateChild() {
		return false
	}
	fi, err := os.Stat(in.TranscriptPath)
	return err == nil && !fi.IsDir()
}

func blockForUpdate(stderr io.Writer, edits int) int {
	fmt.Fprintf(stderr, "Session update (~%d file edits this session): the background worker could not be started. "+
		"Before stopping, run `claude-code memory update` to save the session to the memory bank.\n", edits)
	return core.ExitBlock
}

// updateStamp is "<RFC3339>|<bg|block count|catchup>|<edits covered>".
type updateStamp struct {
	at     time.Time
	bg     bool
	blocks int
	edits  int
	state  string // raw second field: "bg", "catchup", or a block count
}

func stampPath(sessionID string) string {
	return filepath.Join(memstore.StateDir(), "stamp-"+filepath.Base(sessionID))
}

func readStamp(path string) (updateStamp, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return updateStamp{}, false
	}
	parts := strings.Split(strings.TrimSpace(string(b)), "|")
	if len(parts) != 3 {
		return updateStamp{}, false
	}
	at, err := time.Parse(time.RFC3339, parts[0])
	if err != nil {
		return updateStamp{}, false
	}
	st := updateStamp{at: at, bg: parts[1] == "bg", state: parts[1]}
	// parts[1] is "bg", "catchup", or a block count: only the numeric form is a real block count, so a non-numeric state leaves st.blocks at its zero value instead of masquerading as one.
	if n, err := strconv.Atoi(parts[1]); err == nil {
		st.blocks = n
	}
	st.edits, _ = strconv.Atoi(parts[2])
	return st, true
}

func writeStamp(path string, at time.Time, state string, edits int) error {
	err := paths.AtomicWrite(path, []byte(at.Format(time.RFC3339)+"|"+state+"|"+strconv.Itoa(edits)), 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "session-update: write stamp %s: %v\n", path, err)
	}
	return err
}

// countState tracks complete transcript lines separately from edits already covered by an update; subEdits/subBytes cache dispatched subagents' edit count against their summed file size.
type countState struct {
	edits    int
	offset   int64
	subEdits int
	subBytes int64
	turns    int
}

func countPath(sessionID string) string {
	return filepath.Join(memstore.StateDir(), "count-"+filepath.Base(sessionID))
}

func readCountState(path string) (countState, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return countState{}, false
	}
	parts := strings.Split(strings.TrimSpace(string(b)), "|")
	switch len(parts) {
	case 2, 3:
		// Pre-upgrade formats: offset sits past lines the old binary never weighted or turns never counted.
		return countState{}, true
	case 4:
		return parseCountFields(parts)
	}
	return countState{}, false
}

// parseCountFields decodes the current four-field count line: edits|offset|subEdits:subBytes|turns.
func parseCountFields(parts []string) (countState, bool) {
	edits, err1 := strconv.Atoi(parts[0])
	offset, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil || edits < 0 || offset < 0 {
		return countState{}, false
	}
	cs := countState{edits: edits, offset: offset}
	var ok bool
	if cs.subEdits, cs.subBytes, ok = parseSubCache(parts[2]); !ok {
		return countState{}, false
	}
	turns, err := strconv.Atoi(parts[3])
	if err != nil || turns < 0 {
		return countState{}, false
	}
	cs.turns = turns
	return cs, true
}

// parseSubCache decodes the "subEdits:subBytes" field.
func parseSubCache(field string) (subEdits int, subBytes int64, ok bool) {
	subEditsStr, subBytesStr, ok := strings.Cut(field, ":")
	subEdits, err1 := strconv.Atoi(subEditsStr)
	subBytes, err2 := strconv.ParseInt(subBytesStr, 10, 64)
	if !ok || err1 != nil || err2 != nil || subEdits < 0 || subBytes < 0 {
		return 0, 0, false
	}
	return subEdits, subBytes, true
}

func writeCountState(path string, cs countState) {
	line := strconv.Itoa(cs.edits) + "|" + strconv.FormatInt(cs.offset, 10) + "|" +
		strconv.Itoa(cs.subEdits) + ":" + strconv.FormatInt(cs.subBytes, 10) + "|" +
		strconv.Itoa(cs.turns)
	if err := paths.AtomicWrite(path, []byte(line), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "session-update: write count state %s: %v\n", path, err)
	}
}

// countEdits returns work units (edits + user turns/2), resuming after the last complete line; dispatched subagents' files (edits only) are re-scanned only when their summed size moved, never on every Stop.
func countEdits(transcriptPath, countFile string) int {
	return countWork(transcriptPath, countFile, true)
}

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

// subFileState is one subagent file's offset + edits already counted past that offset, cached in the count file's sidecar so a size bump only rescans the bytes appended since the last Stop.
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

// subagentEdits sums agent-*.jsonl sizes under the session's subagents dir and only recounts their edits when that sum moved since the cached value — then only the per-file bytes appended since each file's own cached offset (sidecar ".offsets.json" inside the subagents dir), never the whole file from byte 0. write=false recounts without the lock or sidecar write.
func subagentEdits(transcriptPath string, cachedBytes int64, cachedEdits int, write bool) (edits int, totalBytes int64) {
	dir := inflight.SubagentDir(transcriptPath)
	if dir == "" {
		return 0, 0
	}
	matches, err := filepath.Glob(filepath.Join(dir, "agent-*.jsonl"))
	if err != nil {
		return cachedEdits, cachedBytes // glob failed: keep the last known count rather than reporting zero
	}
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
	// Stop and SessionEnd can both call in for the same session: serialize the read-compute-write so neither writer regresses the other's offsets; a lock timeout keeps hooks fast by skipping the write.
	ran, lockErr := paths.TryFileLock(sidecar+".lock", sidecarLockWait, func() error {
		edits = recountSubagents(matches, sizes, sidecar, true)
		return nil
	})
	if ran {
		return edits, totalBytes
	}
	if lockErr != nil {
		// Lock file couldn't even be opened (e.g. read-only subagents dir): compute unlocked, skip the sidecar write rather than freezing the count forever.
		return recountSubagents(matches, sizes, sidecar, false), totalBytes
	}
	// Timeout from contention: retry on the next call rather than caching the new totalBytes against stale edits, which would make the next call's fast-path skip the recount permanently.
	return cachedEdits, cachedBytes
}

// recountSubagents rescans each file past its cached offset and sums the edits; write controls whether the sidecar is
// persisted, so an unlocked fallback (read-only subagents dir) never writes a stale state.
func recountSubagents(matches []string, sizes map[string]int64, sidecar string, write bool) int {
	prevState := readSidecar(sidecar)
	next := make(map[string]subFileState, len(matches))
	sum := 0
	for _, m := range matches {
		st := prevState[m] // nil map read: zero value, no rescan skipped
		if sizes[m] < st.Offset {
			st = subFileState{} // shrank or rotated: rescan from 0
		}
		if sizes[m] != st.Offset {
			delta, off := transcript.CountEditsFrom(m, st.Offset)
			st.Edits += delta
			st.Offset = off
		}
		sum += st.Edits
		next[m] = st
	}
	if write {
		writeSidecar(sidecar, next)
	}
	return sum
}

func sessionRootInput(in *core.Input) *core.Input {
	copy := *in
	copy.CWD = memrecall.SessionRoot(in.SessionID, in.CWD)
	return &copy
}

// spawnUpdate is the detached-worker seam; tests replace it.
var spawnUpdate = func(transcriptPath, cwd, sessionID string) bool {
	self, err := os.Executable()
	if err != nil {
		return false
	}
	logPath := filepath.Join(memstore.StateDir(), "update-"+time.Now().Format("20060102-150405")+".log")
	if os.MkdirAll(filepath.Dir(logPath), 0o700) != nil {
		return false
	}
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return false
	}
	defer log.Close()
	args := []string{"memory", "update", "--transcript", transcriptPath, "--cwd", cwd, "--session", sessionID}
	if !proc.SpawnDetached(self, args, cwd, log) {
		return false
	}
	_ = memory.WriteUpdateResult(sessionID, memory.UpdateResult{Status: memory.UpdatePending, Time: time.Now(), Log: logPath})
	return true
}

// dispatchUpdate spawns the worker and records it; a Stop's spawn also gets a watcher for the session's final state.
func dispatchUpdate(in *core.Input, trigger string, work int, now time.Time) bool {
	if !spawnUpdate(in.TranscriptPath, in.CWD, in.SessionID) {
		return false
	}
	logUpdateToLedger(in, trigger, work, now)
	_ = writeStamp(stampPath(in.SessionID), now, "bg", work) // best effort: a lost stamp only re-dispatches later
	if trigger == triggerStop {
		spawnIdleWatcher(in)
	}
	return true
}

// refreshEmbeddings schedules only stale vector mints; no endpoint work runs in the hook.
func refreshEmbeddings(cwd string) {
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	if bank.IsUpdateChild() || !memory.HasBank(cwd) {
		return
	}
	if profile := embed.EmbedProfile(); profile != "" {
		embed.RefreshEmbeddings(cwd, profile, time.Now())
	}
}
