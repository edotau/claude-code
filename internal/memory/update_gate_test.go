package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths"
)

// runGate is the update hooks' path minus the hook layer: session root, Updatable, then UpdateIfNew.
func runGate(session, tr, cwd, trigger string) GateResult {
	if !Updatable(session, tr) {
		return GateResult{}
	}
	return UpdateIfNew(UpdateRequest{SessionID: session, Transcript: tr, CWD: SessionRoot(session, cwd)}, trigger, time.Now())
}

// gateRun drives the Stop / SessionEnd gates against one transcript with the detached spawn stubbed.
type gateRun struct {
	t     *testing.T
	tr    string
	cwd   string
	calls *atomic.Int64
}

func newGateRun(t *testing.T) *gateRun {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(UpdateChildEnv, "")
	return &gateRun{t: t, tr: filepath.Join(t.TempDir(), "t.jsonl"), cwd: t.TempDir(), calls: &StubSpawn(t, true).Update}
}

func (u *gateRun) stop() GateResult { return runGate("s1", u.tr, u.cwd, TriggerStop) }

func (u *gateRun) end() { runGate("s1", u.tr, u.cwd, TriggerSessionEnd) }

// wantSpawns fails the test unless exactly want detached updates have been dispatched so far.
func (u *gateRun) wantSpawns(want int64, msg string) {
	u.t.Helper()
	if got := u.calls.Load(); got != want {
		u.t.Errorf("%s: calls=%d, want %d", msg, got, want)
	}
}

// A subagent's own agent-*.jsonl edits count toward the parent session and arm the Stop gate.
func TestCountEditsIncludesSubagentEdits(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(UpdateChildEnv, "")
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	hosttest.WriteEdits(t, tr, 1)
	subDir := filepath.Join(strings.TrimSuffix(tr, ".jsonl"), "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	hosttest.WriteEdits(t, filepath.Join(subDir, "agent-a.jsonl"), 2)

	if got := countEdits(tr, countPath("sub-session")); got != 3 {
		t.Fatalf("countEdits = %d, want 3 (1 main edit + 2 subagent edits)", got)
	}

	cwd := t.TempDir()
	calls := &StubSpawn(t, true).Update
	if g := runGate("sub-session", tr, cwd, TriggerStop); g.Blocked || calls.Load() != 1 {
		t.Fatalf("subagent edits must arm the update gate: gate=%+v calls=%d", g, calls.Load())
	}
}

// One `git commit` Bash call with zero edits weighs updateMinEdits on its own and arms the Stop gate.
func TestCountEditsGitCommitWeightArmsGate(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(UpdateChildEnv, "")
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	commit := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"git commit -m x"}}]}}` + "\n"
	if err := os.WriteFile(tr, []byte(commit), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := countEdits(tr, countPath("commit-session")); got != updateMinEdits {
		t.Fatalf("countEdits = %d, want %d (one git commit)", got, updateMinEdits)
	}

	cwd := t.TempDir()
	calls := &StubSpawn(t, true).Update
	if g := runGate("commit-session", tr, cwd, TriggerStop); g.Blocked || calls.Load() != 1 {
		t.Fatalf("a bare git commit must arm the update gate: gate=%+v calls=%d", g, calls.Load())
	}
}

func TestSessionUpdateTriggerAndReupdate(t *testing.T) {
	u := newGateRun(t)

	hosttest.WriteEdits(t, u.tr, 2)
	if u.stop().Blocked {
		t.Error("under the edit floor must proceed")
	}
	u.wantSpawns(0, "under the edit floor must not spawn")
	hosttest.WriteEdits(t, u.tr, 3)
	if u.stop().Blocked {
		t.Error("3 edits must proceed")
	}
	u.wantSpawns(1, "3 edits must spawn once")
	hosttest.WriteEdits(t, u.tr, 20)
	u.stop()
	u.wantSpawns(1, "re-update inside the cooldown")
	stamp := stampPath("s1")
	writeStamp(stamp, time.Now().Add(-16*time.Minute), "bg", 3)
	hosttest.WriteEdits(t, u.tr, 12)
	u.stop()
	u.wantSpawns(1, "re-update with fewer than 10 new edits")
	hosttest.WriteEdits(t, u.tr, 13)
	u.stop()
	u.wantSpawns(2, "10 new edits after the cooldown must re-update")
	if st, _ := readStamp(stamp); st.state != "bg" || st.edits != 13 {
		t.Errorf("stamp %+v", st)
	}

	// SessionEnd: nothing new since the last dispatch → no spawn; new edits → final update.
	u.end()
	u.wantSpawns(2, "session end with no new edits spawned")
	hosttest.WriteEdits(t, u.tr, 14)
	u.end()
	u.wantSpawns(3, "session end with new edits must spawn")

	t.Setenv(UpdateChildEnv, "1")
	_ = os.Remove(stamp)
	u.stop()
	u.wantSpawns(3, "an update child must never spawn another")
}

func TestSessionUpdateSpawnFailureBlocksThrice(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(UpdateChildEnv, "")
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	cwd := filepath.Join(t.TempDir(), "proj")
	hosttest.WriteEdits(t, tr, 5)
	StubSpawn(t, false)
	blocks := []bool{}
	for range 5 {
		g := runGate("s1", tr, cwd, TriggerStop)
		blocks = append(blocks, g.Blocked)
		if g.Blocked && g.Work != 5 {
			t.Errorf("block work = %d, want the 5 live edits", g.Work)
		}
	}
	if fmt.Sprint(blocks) != fmt.Sprint([]bool{true, true, true, false, false}) {
		t.Errorf("blocks %v", blocks)
	}

	// Evidence releases the gate: a bank write after the stamp.
	_ = os.Remove(stampPath("s1"))
	runGate("s1", tr, cwd, TriggerStop)
	writeStamp(stampPath("s1"), time.Now().Add(-time.Minute), "1", 5)
	if _, err := Init(cwd); err != nil {
		t.Fatal(err)
	}
	if g := runGate("s1", tr, cwd, TriggerStop); g.Blocked {
		t.Error("a bank write after the prompt must release the gate")
	}
}

// A compaction dispatches only uncovered edits, never blocks, and never re-dispatches covered ones.
func TestPreCompactUpdatesOnlyUncoveredEdits(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(UpdateChildEnv, "")
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	cwd := t.TempDir()
	calls := &StubSpawn(t, true).Update
	run := func() bool { return runGate("s1", tr, cwd, TriggerPreCompact).Blocked }

	hosttest.WriteEdits(t, tr, 0)
	if run() || calls.Load() != 0 {
		t.Fatal("no edits must not spawn")
	}
	hosttest.WriteEdits(t, tr, 1) // below the Stop floor: a compaction still checkpoints it
	if run() || calls.Load() != 1 {
		t.Fatalf("uncovered edit must spawn once, calls=%d", calls.Load())
	}
	if run(); calls.Load() != 1 {
		t.Error("covered edits re-dispatched")
	}
	hosttest.WriteEdits(t, tr, 4)
	if run(); calls.Load() != 2 {
		t.Errorf("new edits after the stamp must spawn again, calls=%d", calls.Load())
	}
}

// Discussion-only work crosses the floor on user turns alone: 6 turns are 3 units, exactly updateMinEdits.
func TestSessionUpdateDiscussionOnly(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(UpdateChildEnv, "")
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	cwd := t.TempDir()
	calls := &StubSpawn(t, true).Update
	run := func() bool { return runGate("s1", tr, cwd, TriggerStop).Blocked }

	hosttest.WriteUserTurns(t, tr, 5)
	if run() || calls.Load() != 0 {
		t.Fatalf("5 user turns (2 work units) sit under the floor, calls=%d", calls.Load())
	}
	hosttest.WriteUserTurns(t, tr, 6)
	if run() || calls.Load() != 1 {
		t.Fatalf("6 user turns (3 work units) must spawn once, calls=%d", calls.Load())
	}
	if st, _ := readStamp(stampPath("s1")); st.state != "bg" {
		t.Errorf("stamp %+v, want bg", st)
	}
}

// New user turns past the stamp re-update at pre-compact and session end; nothing new never re-updates.
func TestPreCompactAndSessionEndUpdateNewUserTurns(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(UpdateChildEnv, "")
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	cwd := t.TempDir()
	calls := &StubSpawn(t, true).Update
	gate := func(trigger string) { runGate("s1", tr, cwd, trigger) }

	hosttest.WriteEdits(t, tr, 3)
	if gate(TriggerPreCompact); calls.Load() != 1 {
		t.Fatalf("3 edits must spawn at pre-compact, calls=%d", calls.Load())
	}
	appendUserTurns(t, tr, 4) // the only new work since the stamp: 4 turns = 2 work units
	if gate(TriggerPreCompact); calls.Load() != 2 {
		t.Fatalf("new user turns since the stamp must spawn, calls=%d", calls.Load())
	}
	if gate(TriggerSessionEnd); calls.Load() != 2 {
		t.Error("no new work since the stamp must not re-update")
	}
	appendUserTurns(t, tr, 2)
	if gate(TriggerSessionEnd); calls.Load() != 3 {
		t.Errorf("2 new turns (1 work unit) past the stamp must spawn, calls=%d", calls.Load())
	}
	if gate(TriggerSessionEnd); calls.Load() != 3 {
		t.Error("session end with nothing new re-updated")
	}
}

// A "catchup" stamp parses with blocks 0, and the Stop gate reading it does not crash.
func TestReadStampCatchupStateDoesNotBreakParsing(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	path := filepath.Join(t.TempDir(), "stamp-x")
	at := time.Now().Add(-5 * time.Minute)
	writeStamp(path, at, "catchup", 7)

	st, ok := readStamp(path)
	if !ok || st.state != "catchup" || st.blocks != 0 || st.edits != 7 || !st.at.Equal(at.Truncate(time.Second)) {
		t.Errorf("readStamp(catchup) = %+v ok=%v", st, ok)
	}

	// The Stop gate must not crash reading this stamp for a session that resumes under the same id.
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	hosttest.WriteEdits(t, tr, 20)
	cwd := t.TempDir()
	StubSpawn(t, false)
	runGate("x", tr, cwd, TriggerStop)
}

func TestSessionUpdateUnwritableStampFailsOpen(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(UpdateChildEnv, "")
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	cwd := filepath.Join(t.TempDir(), "proj")
	hosttest.WriteEdits(t, tr, 5)
	StubSpawn(t, false)
	if err := os.MkdirAll(filepath.Join(stampPath("s1"), "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if g := runGate("s1", tr, cwd, TriggerStop); g.Blocked || !g.FailedOpen {
		t.Errorf("an unwritable stamp cannot count blocks, so Stop must fail open; got %+v", g)
	}
}

func appendUserTurns(t *testing.T, path string, n int) {
	t.Helper()
	line := `{"type":"user","message":{"content":"ship it"}}` + "\n"
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(strings.Repeat(line, n)); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// statSize returns path's current size, failing the test when it cannot be stat'ed.
func statSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}

// An invalid count format forces a full rescan; the four-field format round-trips exactly.
func TestCountStateOldFormatReadsAndNewFieldsRoundTrip(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	path := filepath.Join(t.TempDir(), "count-x")
	if err := os.WriteFile(path, []byte("5|120"), 0o600); err != nil {
		t.Fatal(err)
	}
	cs, ok := readCountState(path)
	if ok || cs != (countState{}) {
		t.Fatalf("old-format read = %+v ok=%v, want zero state forcing a full rescan", cs, ok)
	}

	want := countState{edits: 7, offset: 240, subEdits: 4, subBytes: 999, turns: 3}
	writeCountState(path, want)
	got, ok := readCountState(path)
	if !ok || got != want {
		t.Fatalf("round trip = %+v ok=%v, want %+v", got, ok, want)
	}
}

// An old 2-field count file forces a full rescan, so a past `git commit` is weighted and the file gets 4 fields.
func TestCountEditsOldFormatCountFileRescansUnweightedCommit(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	commit := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"git commit -m x"}}]}}` + "\n"
	if err := os.WriteFile(tr, []byte(commit), 0o600); err != nil {
		t.Fatal(err)
	}
	countFile := filepath.Join(t.TempDir(), "count-old")
	if err := os.WriteFile(countFile, fmt.Appendf(nil, "0|%d", len(commit)), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := countEdits(tr, countFile); got != updateMinEdits {
		t.Fatalf("countEdits after old-format upgrade = %d, want %d (the commit, now weighted)", got, updateMinEdits)
	}
	raw, err := os.ReadFile(countFile)
	if err != nil {
		t.Fatal(err)
	}
	if parts := strings.Split(strings.TrimSpace(string(raw)), "|"); len(parts) != 4 {
		t.Fatalf("count file after upgrade = %q, want the 4-field format", raw)
	}
}

// A 3-field count file forces a full rescan, so its uncounted user turns are weighted and the file gets 4 fields.
func TestCountEditsThreeFieldCountFileRescansTurns(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	hosttest.WriteUserTurns(t, tr, 4)
	fi, err := os.Stat(tr)
	if err != nil {
		t.Fatal(err)
	}
	countFile := filepath.Join(t.TempDir(), "count-3field")
	// Pre-upgrade "edits|offset|subEdits:subBytes": offset past every (uncounted) user turn.
	if err := os.WriteFile(countFile, fmt.Appendf(nil, "0|%d|0:0", fi.Size()), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := countEdits(tr, countFile); got != 2 { // 4 turns / 2 per work unit
		t.Fatalf("countEdits after 3-field upgrade = %d, want 2 (the 4 turns, now weighted)", got)
	}
	raw, err := os.ReadFile(countFile)
	if err != nil {
		t.Fatal(err)
	}
	if parts := strings.Split(strings.TrimSpace(string(raw)), "|"); len(parts) != 4 {
		t.Fatalf("count file after upgrade = %q, want the 4-field format", raw)
	}
}

// subagentEdits trusts the cached count while the summed subagent size is unchanged and rescans once it grows.
func TestCountSubagentEditsCachesUnchangedSizeRecountsGrowth(t *testing.T) {
	dir := t.TempDir()
	tr := filepath.Join(dir, "t.jsonl")
	subDir := filepath.Join(strings.TrimSuffix(tr, ".jsonl"), "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	agent := filepath.Join(subDir, "agent-a.jsonl")
	hosttest.WriteEdits(t, agent, 2)
	size := statSize(t, agent)

	edits, bytes := subagentEdits(tr, 0, 0, true)
	if edits != 2 || bytes != size {
		t.Fatalf("initial scan = (%d,%d), want (2,%d)", edits, bytes, size)
	}

	// A wrong cached count with the SAME size must come back as-is: proof the rescan was skipped, not repeated.
	edits2, bytes2 := subagentEdits(tr, size, 999, true)
	if edits2 != 999 || bytes2 != size {
		t.Fatalf("unchanged size must skip the rescan: got (%d,%d), want (999,%d)", edits2, bytes2, size)
	}

	hosttest.WriteEdits(t, agent, 5) // grows the file: the cached size no longer matches
	size2 := statSize(t, agent)
	if size2 == size {
		t.Fatal("test setup: file did not grow")
	}
	edits3, bytes3 := subagentEdits(tr, size, 999, true)
	if edits3 != 5 || bytes3 != size2 {
		t.Fatalf("grown file must be rescanned: got (%d,%d), want (5,%d)", edits3, bytes3, size2)
	}
}

// wantSidecarEdits fails unless every listed file's sidecar entry records exactly the wanted edit count.
func wantSidecarEdits(t *testing.T, sc map[string]subFileState, want map[string]int) {
	t.Helper()
	for path, edits := range want {
		if sc[path].Edits != edits {
			t.Fatalf("sidecar entry for %s = %+v, want %d edits (sidecar %+v)", filepath.Base(path), sc[path], edits, sc)
		}
	}
}

// subagentFixture returns a fresh session transcript path + its subagents dir, created on disk.
func subagentFixture(t *testing.T) (tr, subDir string) {
	t.Helper()
	dir := t.TempDir()
	tr = filepath.Join(dir, "t.jsonl")
	subDir = filepath.Join(strings.TrimSuffix(tr, ".jsonl"), "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return tr, subDir
}

// Growing one of three subagent files advances only that file's sidecar entry.
func TestCountSubagentEditsGrowOneFileOnlyAdvancesItsSidecarEntry(t *testing.T) {
	tr, subDir := subagentFixture(t)
	a := filepath.Join(subDir, "agent-a.jsonl")
	b := filepath.Join(subDir, "agent-b.jsonl")
	c := filepath.Join(subDir, "agent-c.jsonl")
	hosttest.WriteEdits(t, a, 1)
	hosttest.WriteEdits(t, b, 2)
	hosttest.WriteEdits(t, c, 3)

	edits, totalBytes := subagentEdits(tr, 0, 0, true)
	if want := 1 + 2 + 3; edits != want {
		t.Fatalf("initial scan edits = %d, want %d", edits, want)
	}
	sidecar := sidecarPath(subDir)
	before := readSidecar(sidecar)
	wantSidecarEdits(t, before, map[string]int{a: 1, b: 2, c: 3})

	hosttest.WriteEdits(t, a, 4) // only a grows (overwritten with more lines, same identical-line trick as above)
	size := statSize(t, a)

	edits2, totalBytes2 := subagentEdits(tr, totalBytes, edits, true)
	if totalBytes2 == totalBytes {
		t.Fatal("test setup: total size did not move")
	}
	if want := 4 + 2 + 3; edits2 != want {
		t.Fatalf("after growing a: edits = %d, want %d", edits2, want)
	}
	after := readSidecar(sidecar)
	wantSidecarEdits(t, after, map[string]int{a: 4})
	if after[a].Offset != size {
		t.Fatalf("grown file a's sidecar entry = %+v, want offset %d", after[a], size)
	}
	if after[b] != before[b] || after[c] != before[c] {
		t.Fatalf("sibling sidecar entries moved: b=%+v (was %+v), c=%+v (was %+v)", after[b], before[b], after[c], before[c])
	}
}

// A sidecar offset past the file's size (truncated or rotated) rescans that file from 0.
func TestCountSubagentEditsOffsetPastFileSizeRescansFromZero(t *testing.T) {
	tr, subDir := subagentFixture(t)
	a := filepath.Join(subDir, "agent-a.jsonl")
	hosttest.WriteEdits(t, a, 3)
	fi, err := os.Stat(a)
	if err != nil {
		t.Fatal(err)
	}
	writeSidecar(sidecarPath(subDir), map[string]subFileState{a: {Offset: fi.Size() + 1_000, Edits: 999}})

	edits, totalBytes := subagentEdits(tr, 0, 0, true) // cachedBytes=0 forces the rescan path
	if edits != 3 || totalBytes != fi.Size() {
		t.Fatalf("stale-offset rescan = (%d,%d), want (3,%d)", edits, totalBytes, fi.Size())
	}
	got := readSidecar(sidecarPath(subDir))
	if got[a].Edits != 3 || got[a].Offset != fi.Size() {
		t.Fatalf("sidecar after stale-offset rescan = %+v, want 3 edits at offset %d", got[a], fi.Size())
	}
}

// Unparsable sidecar JSON never panics and reads as no cache: every file rescans from 0.
func TestCountSubagentEditsGarbageSidecarTreatedAsEmpty(t *testing.T) {
	tr, subDir := subagentFixture(t)
	a := filepath.Join(subDir, "agent-a.jsonl")
	hosttest.WriteEdits(t, a, 2)
	if err := os.WriteFile(sidecarPath(subDir), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	edits, totalBytes := subagentEdits(tr, 0, 0, true)
	fi, err := os.Stat(a)
	if err != nil {
		t.Fatal(err)
	}
	if edits != 2 || totalBytes != fi.Size() {
		t.Fatalf("garbage-sidecar scan = (%d,%d), want (2,%d)", edits, totalBytes, fi.Size())
	}
	got := readSidecar(sidecarPath(subDir))
	if got[a].Edits != 2 {
		t.Fatalf("sidecar rewritten after garbage input = %+v, want 2 edits for a", got[a])
	}
}

// A subagent file deleted between Stops drops out of the rewritten sidecar.
func TestCountSubagentEditsDeletedFileDropsFromSidecar(t *testing.T) {
	tr, subDir := subagentFixture(t)
	a := filepath.Join(subDir, "agent-a.jsonl")
	b := filepath.Join(subDir, "agent-b.jsonl")
	hosttest.WriteEdits(t, a, 1)
	hosttest.WriteEdits(t, b, 2)

	edits, totalBytes := subagentEdits(tr, 0, 0, true)
	if edits != 3 {
		t.Fatalf("initial scan edits = %d, want 3", edits)
	}
	if err := os.Remove(a); err != nil {
		t.Fatal(err)
	}
	hosttest.WriteEdits(t, b, 5) // move the total size so the cache is invalidated and a rescan actually runs

	edits2, totalBytes2 := subagentEdits(tr, totalBytes, edits, true)
	if totalBytes2 == totalBytes {
		t.Fatal("test setup: total size did not move")
	}
	if edits2 != 5 {
		t.Fatalf("after deleting a: edits = %d, want 5 (b only)", edits2)
	}
	got := readSidecar(sidecarPath(subDir))
	if _, present := got[a]; present {
		t.Fatalf("deleted file a still present in rewritten sidecar: %+v", got)
	}
	if _, present := got[b]; !present {
		t.Fatalf("surviving file b missing from rewritten sidecar: %+v", got)
	}
}

// Two concurrent counts of one growth serialize on the sidecar lock: counted once, one final offset.
func TestCountSubagentEditsConcurrentCallsCountOnce(t *testing.T) {
	tr, subDir := subagentFixture(t)
	a := filepath.Join(subDir, "agent-a.jsonl")
	hosttest.WriteEdits(t, a, 2)

	edits0, bytes0 := subagentEdits(tr, 0, 0, true) // seed the sidecar
	if edits0 != 2 {
		t.Fatalf("seed scan edits = %d, want 2", edits0)
	}

	hosttest.WriteEdits(t, a, 7) // grows past the cached size: both goroutines will try the lock
	fi, err := os.Stat(a)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	results := make([]struct {
		edits int
		bytes int64
	}, 2)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			e, b := subagentEdits(tr, bytes0, edits0, true)
			results[i] = struct {
				edits int
				bytes int64
			}{e, b}
		}(i)
	}
	wg.Wait()

	for i, r := range results {
		if r.edits != 7 || r.bytes != fi.Size() {
			t.Errorf("goroutine %d = (%d,%d), want (7,%d) — the race must not double- or under-count", i, r.edits, r.bytes, fi.Size())
		}
	}
	got := readSidecar(sidecarPath(subDir))
	if got[a].Edits != 7 || got[a].Offset != fi.Size() {
		t.Fatalf("sidecar after concurrent calls = %+v, want 7 edits at offset %d (one writer, consistent offset)", got[a], fi.Size())
	}
}

// With the sidecar lock held elsewhere subagentEdits returns the cached totals, writes nothing and never hangs.
func TestCountSubagentEditsLockHeldElsewhereReturnsCachedWithoutWriting(t *testing.T) {
	oldWait := sidecarLockWait
	sidecarLockWait = 100 * time.Millisecond
	t.Cleanup(func() { sidecarLockWait = oldWait })
	tr, subDir := subagentFixture(t)
	a := filepath.Join(subDir, "agent-a.jsonl")
	hosttest.WriteEdits(t, a, 2)
	edits0, bytes0 := subagentEdits(tr, 0, 0, true) // seed the sidecar
	before := readSidecar(sidecarPath(subDir))

	hosttest.WriteEdits(t, a, 9) // grows past the cached size: subagentEdits must attempt the lock

	held := make(chan struct{})
	release := make(chan struct{})
	go func() {
		paths.WithFileLock(sidecarPath(subDir), func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	defer close(release)

	start := time.Now()
	edits, bytes := subagentEdits(tr, bytes0, edits0, true)
	elapsed := time.Since(start)

	// Busy lock keeps both cached totals: new bytes against stale edits would skip the recount forever.
	if edits != edits0 || bytes != bytes0 {
		t.Fatalf("lock held elsewhere: subagentEdits = (%d,%d), want (%d,%d)", edits, bytes, edits0, bytes0)
	}
	if elapsed > time.Second {
		t.Fatalf("subagentEdits took %s waiting on a held lock, want it to give up near its %s wait", elapsed, sidecarLockWait)
	}
	after := readSidecar(sidecarPath(subDir))
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("sidecar written while the lock was held elsewhere: before=%+v after=%+v", before, after)
	}
}

// TestReadOnlyEditsMatchesCountEditsWithoutWriting: the dry-run count includes subagent edits and persists nothing.
func TestReadOnlyEditsMatchesCountEditsWithoutWriting(t *testing.T) {
	dir := t.TempDir()
	tr := filepath.Join(dir, "t.jsonl")
	hosttest.WriteEdits(t, tr, 1)
	subDir := filepath.Join(strings.TrimSuffix(tr, ".jsonl"), "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	hosttest.WriteEdits(t, filepath.Join(subDir, "agent-a.jsonl"), 3)
	countFile := filepath.Join(dir, "count")

	dry := readOnlyEdits(tr, countFile)
	if entries, _ := os.ReadDir(subDir); len(entries) != 1 {
		t.Errorf("dry count wrote into the subagents dir: %v", entries)
	}
	if _, err := os.Stat(countFile); err == nil {
		t.Error("dry count wrote the count file")
	}
	if real := countEdits(tr, countFile); dry != 4 || real != dry {
		t.Errorf("dry = %d, real = %d, want both 4", dry, real)
	}
}

// Regression: a filepath.Glob result must reach the count, never be swallowed into an empty file list.
func TestCountSubagentEditsHandlesGlobError(t *testing.T) {
	// Create a transcript path with no subagents dir — Glob will return no matches (ok state).
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	hosttest.WriteEdits(t, tr, 1)

	// No subagents dir exists — Glob should handle this gracefully.
	edits, bytes := subagentEdits(tr, 0, 0, true)
	if edits != 0 || bytes != 0 {
		t.Errorf("subagentEdits with no subagents dir = (%d, %d), want (0, 0)", edits, bytes)
	}

	// Now create a subagents dir with agent files.
	subDir := filepath.Join(strings.TrimSuffix(tr, ".jsonl"), "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	hosttest.WriteEdits(t, filepath.Join(subDir, "agent-a.jsonl"), 3)

	edits2, _ := subagentEdits(tr, 0, 0, true)
	if edits2 != 3 {
		t.Errorf("subagentEdits after adding agent files = %d, want 3", edits2)
	}
}
