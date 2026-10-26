package hooks

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/transcript"
)

// Harvest gate thresholds: the floor asks "was this substantive work"; re-arming needs 10 NEW edits AND 15 min,
// since every harvest appends a sessionHistory entry and a chatty re-arm evicts other sessions' history.
const (
	harvestMinEdits   = transcript.CommitEditWeight // keeps a `git commit` worth exactly one gate floor
	harvestRearmEdits = 10
	reharvestCooldown = 15 * time.Minute
	harvestMaxBlocks  = 3
)

// SessionStart injects the memory bank (auto-scaffolding it for safe roots) as additionalContext.
func SessionStart(r io.Reader, stdout io.Writer) int {
	in := ParseInput(r)
	cwd := in.CWD
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	ctx := memoryContext(cwd, in.SessionID, in.TranscriptPath, time.Now())
	if out := MergeContext("SessionStart", []string{ctx}); out != "" {
		fmt.Fprintln(stdout, out)
	}
	return ExitProceed
}

// memoryContext renders the note line + bank surface and advances the session chain.
func memoryContext(cwd, sessionID, transcriptPath string, now time.Time) string {
	guard := memory.StartupGuard()
	prefix := guard.Notice // paused: the notice ("" otherwise); else a fresh-scaffold notice or ""
	if !guard.Paused {
		prefix = maybeAutoInit(cwd)
	}
	if !guard.Paused && !memory.IsHarvestChild() {
		memory.LinkNativeFor(cwd) // after the scaffold, so the link targets the store Init named (origin record in the vault)
	}
	chain := filepath.Join(memory.StateDir(), "last-session")
	out := guard.Notice // paused with no bank: the notice alone; an existing surface is still injected read-only
	if bank := memory.SessionSurface(cwd); bank != "" {
		note := bankLoadNote(prefix) + chainNote(chain, sessionID, now, guard) + doctorNotice()
		out = note + "\n\n" + bank
	} else if !guard.Paused && prefix != "" {
		out = prefix // a fresh scaffold has no surface yet; still say where the bank went
	}
	// The daily dream (promote + consolidate) runs detached on the Copilot CLI; never blocks launch.
	if !guard.Paused && !memory.IsHarvestChild() && memory.HasBank(cwd) && memory.DreamDue(cwd, now) {
		spawnDream(cwd)
	}
	// Every SessionStart (startup, resume, clear, compact) restarts what recall treats as already in context.
	if !memory.IsHarvestChild() {
		memory.ResetSeen(sessionID, cwd, out)
	}
	// A harvest worker is a session too; it must not become the "previous session" of the user's next one.
	// Non-UUID ids (probe/test payloads) must not clobber the real previous-session pointer.
	if memory.LooksLikeSessionID(sessionID) && !memory.IsHarvestChild() {
		writeChain(chain, chainEntry{sessionID: sessionID, transcriptPath: transcriptPath, cwd: cwd})
	}
	return out
}

// bankLoadNote is the fixed "session memory bank loaded" line, with an optional scaffold/pause prefix.
func bankLoadNote(prefix string) string {
	note := "Session memory bank (local + shared home) loaded below. It auto-saves at session end; `claude-code memory update` is the manual override."
	if prefix != "" {
		note = prefix + " " + note
	}
	return note
}

// chainNote appends the previous-session pointer and any catch-up/attention note, or "" with no chain entry.
func chainNote(chain, sessionID string, now time.Time, guard memory.Guard) string {
	entry, ok := readChain(chain)
	if !ok {
		return ""
	}
	prevID := entry.sessionID
	note := " Previous session id: " + prevID + " (reference for the prior session's transcript / claude --resume)."
	caughtUp := ""
	if !guard.Paused && !memory.IsHarvestChild() {
		caughtUp = catchUpPrevious(entry, sessionID, now)
	}
	if caughtUp != "" {
		note += " " + caughtUp
	} else if res, ok := memory.ReadHarvestResult(prevID); ok && res.NeedsAttention(now) {
		hint := ""
		if res.Log != "" {
			hint = " (log: " + res.Log + ")"
		}
		note += " NOTE: the previous session's memory harvest did not complete (" + res.Status + ")" + hint +
			" — run `claude-code memory update` to recover it."
	}
	return note
}

// doctorNotice is the cached memory-review notice line (from `memory update`'s settle path), or "" with none.
// A full Doctor pass tokenizes the home bank + access log, which has no place on the launch path.
func doctorNotice() string {
	data, err := os.ReadFile(memory.DoctorNoticePath())
	if err != nil {
		return ""
	}
	notice := strings.TrimSpace(string(data))
	if notice == "" {
		return ""
	}
	if len(notice) > 200 {
		notice = notice[:200]
	}
	return " memory review: " + notice + "."
}

// chainEntry is the session chain file: id (line 1), transcript path (line 2), cwd (line 3) —
// lines 2-3 are optional so a pre-upgrade one-line file still parses.
type chainEntry struct {
	sessionID      string
	transcriptPath string
	cwd            string
}

func readChain(path string) (chainEntry, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return chainEntry{}, false
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	id := strings.TrimSpace(lines[0])
	if id == "" {
		return chainEntry{}, false
	}
	e := chainEntry{sessionID: id}
	if len(lines) > 1 {
		e.transcriptPath = strings.TrimSpace(lines[1])
	}
	if len(lines) > 2 {
		e.cwd = strings.TrimSpace(lines[2])
	}
	return e, true
}

func writeChain(path string, e chainEntry) {
	_ = paths.AtomicWrite(path, []byte(e.sessionID+"\n"+e.transcriptPath+"\n"+e.cwd+"\n"), 0o600)
}

// catchUpPrevious spawns the previous session's missed or failed harvest at most once, before its
// chain slot is overwritten; a transcript still growing elsewhere (parallel sessions) is left alone.
func catchUpPrevious(prev chainEntry, sessionID string, now time.Time) string {
	if prev.sessionID == "" || prev.sessionID == sessionID || prev.transcriptPath == "" {
		return ""
	}
	fi, err := os.Stat(prev.transcriptPath)
	if err != nil || now.Sub(fi.ModTime()) < reharvestCooldown {
		return ""
	}
	stamp := stampPath(prev.sessionID)
	st, ok := readStamp(stamp)
	covered := 0
	if ok {
		covered = st.edits
	}
	edits := countEdits(prev.transcriptPath, countPath(prev.sessionID))
	uncovered := edits >= harvestMinEdits && edits > covered
	res, hasResult := memory.ReadHarvestResult(prev.sessionID)
	alreadyRetried := ok && st.state == "catchup"
	failed := hasResult && res.NeedsAttention(now) && !alreadyRetried
	if !uncovered && !failed {
		return ""
	}
	if !spawnHarvest(prev.transcriptPath, prev.cwd, prev.sessionID) {
		return ""
	}
	writeStamp(stamp, now, "catchup", edits)
	return "Catching up the previous session's memory harvest in the background."
}

// maybeAutoInit scaffolds a bank for bank-less safe roots; returns a notice on a fresh scaffold.
func maybeAutoInit(cwd string) string {
	if cwd == "" || memory.HasBank(cwd) || !scaffoldSafe(cwd) {
		return ""
	}
	dir, err := memory.Init(cwd)
	if err != nil {
		return ""
	}
	return "Memory bank scaffolded at " + dir + " — it fills automatically at session end (optionally enrich with `claude-code memory init`)."
}

// scaffoldSafe: the config dir, a dir already carrying .claude/, or a git TOPLEVEL (a subdir would misplace the bank).
func scaffoldSafe(cwd string) bool {
	if paths.SamePath(cwd, paths.ConfigDir()) {
		return true
	}
	if fi, err := os.Stat(filepath.Join(cwd, ".claude")); err == nil && fi.IsDir() {
		return true
	}
	top := memory.GitToplevel(cwd)
	return top != "" && paths.SamePath(top, cwd)
}

// SessionHarvest (Stop) dispatches a detached `memory update` once a session has substantive edits;
// with no spawn possible it blocks at most harvestMaxBlocks times asking for `claude-code memory update`.
func SessionHarvest(r io.Reader, stderr io.Writer) int {
	in := ParseInput(r)
	if !harvestable(in) {
		return ExitProceed
	}
	if memory.StartupGuard().Paused {
		return ExitProceed // maintenance/migration: no spawn, no block, no stamp
	}
	stamp := stampPath(in.SessionID)
	counted := countPath(in.SessionID)
	now := time.Now()
	st, ok := readStamp(stamp)
	switch {
	case !ok:
		edits := countEdits(in.TranscriptPath, counted)
		if edits < harvestMinEdits {
			return ExitProceed
		}
		if spawnHarvest(in.TranscriptPath, in.CWD, in.SessionID) {
			writeStamp(stamp, now, "bg", edits)
			spawnIdleWatcher(in) // the session is harvest-worthy: its final state gets a watcher too
			return ExitProceed
		}
		writeStamp(stamp, now, "1", edits)
		return blockForHarvest(stderr, edits)
	case st.bg:
		if now.Sub(st.at) < reharvestCooldown {
			return ExitProceed // cheap gate before the whole-transcript count
		}
		edits := countEdits(in.TranscriptPath, counted)
		if edits >= st.edits+harvestRearmEdits && spawnHarvest(in.TranscriptPath, in.CWD, in.SessionID) {
			writeStamp(stamp, now, "bg", edits)
			spawnIdleWatcher(in)
		}
		return ExitProceed
	case memory.NewestMtime(memory.Dir(in.CWD)).After(st.at):
		writeStamp(stamp, now, "bg", st.edits) // `claude-code memory update` ran by hand: released, re-arms like a dispatch
		return ExitProceed
	case st.blocks >= harvestMaxBlocks:
		return ExitProceed // fail open
	}
	writeStamp(stamp, st.at, strconv.Itoa(st.blocks+1), st.edits)
	return blockForHarvest(stderr, st.edits)
}

// SessionHarvestEnd (SessionEnd) harvests the final state when edits since the last dispatch cross the floor.
func SessionHarvestEnd(r io.Reader) int {
	in := ParseInput(r)
	if !harvestable(in) {
		return ExitProceed
	}
	if memory.StartupGuard().Paused {
		return ExitProceed
	}
	stamp := stampPath(in.SessionID)
	covered := 0
	if st, ok := readStamp(stamp); ok {
		covered = st.edits
	}
	edits := countEdits(in.TranscriptPath, countPath(in.SessionID))
	if edits >= harvestMinEdits && edits > covered && spawnHarvest(in.TranscriptPath, in.CWD, in.SessionID) {
		writeStamp(stamp, time.Now(), "bg", edits)
	}
	return ExitProceed
}

// PreCompact (ADK add_events_to_memory): before Claude Code compacts the context, harvest any edits no earlier
// dispatch covered so the bank holds them; the transcript file itself survives compaction, so this never blocks.
func PreCompact(r io.Reader) int {
	in := ParseInput(r)
	if !harvestable(in) || memory.StartupGuard().Paused {
		return ExitProceed
	}
	stamp := stampPath(in.SessionID)
	st, ok := readStamp(stamp)
	edits := countEdits(in.TranscriptPath, countPath(in.SessionID))
	if edits == 0 || (ok && edits <= st.edits) {
		return ExitProceed
	}
	if spawnHarvest(in.TranscriptPath, in.CWD, in.SessionID) {
		writeStamp(stamp, time.Now(), "bg", edits)
	}
	return ExitProceed
}

func harvestable(in *Input) bool {
	if in.SessionID == "" || in.TranscriptPath == "" || memory.IsHarvestChild() {
		return false
	}
	fi, err := os.Stat(in.TranscriptPath)
	return err == nil && !fi.IsDir()
}

func blockForHarvest(stderr io.Writer, edits int) int {
	fmt.Fprintf(stderr, "Session harvest (~%d file edits this session): the background worker could not be started. "+
		"Before stopping, run `claude-code memory update` to save the session to the memory bank.\n", edits)
	return ExitBlock
}

// spawnHarvest is the detached-worker seam; tests replace it.
var spawnHarvest = func(transcriptPath, cwd, sessionID string) bool {
	self, err := os.Executable()
	if err != nil {
		return false
	}
	logPath := filepath.Join(memory.StateDir(), "harvest-"+time.Now().Format("20060102-150405")+".log")
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
	_ = memory.WriteHarvestResult(sessionID, memory.HarvestResult{Status: memory.HarvestPending, Time: time.Now(), Log: logPath})
	return true
}

// spawnDream is the detached memory-dream seam; tests replace it.
var spawnDream = func(cwd string) bool {
	self, err := os.Executable()
	if err != nil {
		return false
	}
	logPath := filepath.Join(memory.StateDir(), "dream-"+time.Now().Format("20060102-150405")+".log")
	if os.MkdirAll(filepath.Dir(logPath), 0o700) != nil {
		return false
	}
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return false
	}
	defer log.Close()
	return proc.SpawnDetached(self, []string{"memory", "dream", "--cwd", cwd}, cwd, log)
}

// harvestStamp is "<RFC3339>|<bg|block count|catchup>|<edits covered>".
type harvestStamp struct {
	at     time.Time
	bg     bool
	blocks int
	edits  int
	state  string // raw second field: "bg", "catchup", or a block count
}

func stampPath(sessionID string) string {
	return filepath.Join(memory.StateDir(), "stamp-"+filepath.Base(sessionID))
}

func readStamp(path string) (harvestStamp, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return harvestStamp{}, false
	}
	parts := strings.Split(strings.TrimSpace(string(b)), "|")
	if len(parts) != 3 {
		return harvestStamp{}, false
	}
	at, err := time.Parse(time.RFC3339, parts[0])
	if err != nil {
		return harvestStamp{}, false
	}
	st := harvestStamp{at: at, bg: parts[1] == "bg", state: parts[1]}
	st.blocks, _ = strconv.Atoi(parts[1])
	st.edits, _ = strconv.Atoi(parts[2])
	return st, true
}

func writeStamp(path string, at time.Time, state string, edits int) {
	_ = paths.AtomicWrite(path, []byte(at.Format(time.RFC3339)+"|"+state+"|"+strconv.Itoa(edits)), 0o600)
}

// countState tracks complete transcript lines separately from edits already covered by a harvest;
// subEdits/subBytes cache dispatched subagents' edit count against their summed file size.
type countState struct {
	edits    int
	offset   int64
	subEdits int
	subBytes int64
}

func countPath(sessionID string) string {
	return filepath.Join(memory.StateDir(), "count-"+filepath.Base(sessionID))
}

func readCountState(path string) (countState, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return countState{}, false
	}
	parts := strings.Split(strings.TrimSpace(string(b)), "|")
	if len(parts) == 2 {
		// Pre-upgrade "edits|offset" format: its offset sits past commit lines the old binary never
		// weighted (CommitEditWeight). Force a full rescan from 0; countEdits writes the 3-field form.
		return countState{}, true
	}
	if len(parts) != 3 {
		return countState{}, false
	}
	edits, err1 := strconv.Atoi(parts[0])
	offset, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil || edits < 0 || offset < 0 {
		return countState{}, false
	}
	cs := countState{edits: edits, offset: offset}
	subEditsStr, subBytesStr, ok := strings.Cut(parts[2], ":")
	subEdits, err3 := strconv.Atoi(subEditsStr)
	subBytes, err4 := strconv.ParseInt(subBytesStr, 10, 64)
	if !ok || err3 != nil || err4 != nil || subEdits < 0 || subBytes < 0 {
		return countState{}, false
	}
	cs.subEdits, cs.subBytes = subEdits, subBytes
	return cs, true
}

func writeCountState(path string, cs countState) {
	line := strconv.Itoa(cs.edits) + "|" + strconv.FormatInt(cs.offset, 10) + "|" +
		strconv.Itoa(cs.subEdits) + ":" + strconv.FormatInt(cs.subBytes, 10)
	_ = paths.AtomicWrite(path, []byte(line), 0o600)
}

// countEdits resumes after the last complete line, leaving partial appends for the next Stop; dispatched
// subagents' files are only re-scanned when their summed size moved, never on every Stop.
func countEdits(transcriptPath, countFile string) int {
	fi, err := os.Stat(transcriptPath)
	if err != nil || fi.IsDir() {
		return 0
	}
	cs, _ := readCountState(countFile)
	if fi.Size() < cs.offset {
		_ = os.Remove(countFile)
		cs = countState{}
	}
	mainProgressed := false
	if fi.Size() != cs.offset {
		delta, next := transcript.CountEditsFrom(transcriptPath, cs.offset)
		if next < cs.offset {
			_ = os.Remove(countFile)
			cs = countState{}
		}
		cs.edits += delta
		if next != cs.offset {
			cs.offset = next
			mainProgressed = true
		}
	}
	subEdits, subBytes := countSubagentEdits(transcriptPath, cs.subBytes, cs.subEdits)
	if mainProgressed || subEdits != cs.subEdits || subBytes != cs.subBytes {
		cs.subEdits, cs.subBytes = subEdits, subBytes
		writeCountState(countFile, cs)
	}
	return cs.edits + subEdits
}

// subFileState is one subagent file's offset + edits already counted past that offset, cached in the
// count file's sidecar so a size bump only rescans the bytes appended since the last Stop.
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

// countSubagentEdits sums agent-*.jsonl sizes under the session's subagents dir and only recounts their
// edits when that sum moved since the cached value — then only the per-file bytes appended since each
// file's own cached offset (sidecar ".offsets.json" inside the subagents dir), never the whole file from
// byte 0.
func countSubagentEdits(transcriptPath string, cachedBytes int64, cachedEdits int) (edits int, totalBytes int64) {
	dir := inflight.SubagentDir(transcriptPath)
	if dir == "" {
		return 0, 0
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "agent-*.jsonl"))
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
	// recount rescans each file past its cached offset and sums the edits; write controls whether the
	// sidecar is persisted, so an unlocked fallback (read-only subagents dir) never writes a stale state.
	recount := func(write bool) int {
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
	// Stop and SessionEnd can both call in for the same session: serialize the read-compute-write so
	// neither writer regresses the other's offsets; a lock timeout keeps hooks fast by skipping the write.
	ran, lockErr := paths.TryFileLock(sidecar+".lock", 2*time.Second, func() error {
		edits = recount(true)
		return nil
	})
	if ran {
		return edits, totalBytes
	}
	if lockErr != nil {
		// Lock file couldn't even be opened (e.g. read-only subagents dir): compute unlocked, skip the
		// sidecar write rather than freezing the count forever.
		return recount(false), totalBytes
	}
	// Timeout from contention: retry on the next call rather than caching the new totalBytes against
	// stale edits, which would make the next call's fast-path skip the recount permanently.
	return cachedEdits, cachedBytes
}
