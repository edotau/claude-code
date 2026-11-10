package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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
	return &gateRun{t: t, tr: filepath.Join(t.TempDir(), "t.jsonl"), cwd: t.TempDir(), calls: &stubSpawn(t, true).Update}
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
	writeEdits(t, tr, 1)
	subDir := filepath.Join(strings.TrimSuffix(tr, ".jsonl"), "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeEdits(t, filepath.Join(subDir, "agent-a.jsonl"), 2)

	if got := countEdits(tr, countPath("sub-session")); got != 3 {
		t.Fatalf("countEdits = %d, want 3 (1 main edit + 2 subagent edits)", got)
	}

	cwd := t.TempDir()
	calls := &stubSpawn(t, true).Update
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
	calls := &stubSpawn(t, true).Update
	if g := runGate("commit-session", tr, cwd, TriggerStop); g.Blocked || calls.Load() != 1 {
		t.Fatalf("a bare git commit must arm the update gate: gate=%+v calls=%d", g, calls.Load())
	}
}

func TestSessionUpdateTriggerAndReupdate(t *testing.T) {
	u := newGateRun(t)

	writeEdits(t, u.tr, 2)
	if u.stop().Blocked {
		t.Error("under the edit floor must proceed")
	}
	u.wantSpawns(0, "under the edit floor must not spawn")
	writeEdits(t, u.tr, 3)
	if u.stop().Blocked {
		t.Error("3 edits must proceed")
	}
	u.wantSpawns(1, "3 edits must spawn once")
	writeEdits(t, u.tr, 20)
	u.stop()
	u.wantSpawns(1, "re-update inside the cooldown")
	stamp := stampPath("s1")
	writeStamp(stamp, time.Now().Add(-16*time.Minute), "bg", 3)
	writeEdits(t, u.tr, 12)
	u.stop()
	u.wantSpawns(1, "re-update with fewer than 10 new edits")
	writeEdits(t, u.tr, 13)
	u.stop()
	u.wantSpawns(2, "10 new edits after the cooldown must re-update")
	if st, _ := readStamp(stamp); st.state != "bg" || st.edits != 13 {
		t.Errorf("stamp %+v", st)
	}

	// SessionEnd: nothing new since the last dispatch → no spawn; new edits → final update.
	u.end()
	u.wantSpawns(2, "session end with no new edits spawned")
	writeEdits(t, u.tr, 14)
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
	writeEdits(t, tr, 5)
	stubSpawn(t, false)
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
	calls := &stubSpawn(t, true).Update
	run := func() bool { return runGate("s1", tr, cwd, TriggerPreCompact).Blocked }

	writeEdits(t, tr, 0)
	if run() || calls.Load() != 0 {
		t.Fatal("no edits must not spawn")
	}
	writeEdits(t, tr, 1) // below the Stop floor: a compaction still checkpoints it
	if run() || calls.Load() != 1 {
		t.Fatalf("uncovered edit must spawn once, calls=%d", calls.Load())
	}
	if run(); calls.Load() != 1 {
		t.Error("covered edits re-dispatched")
	}
	writeEdits(t, tr, 4)
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
	calls := &stubSpawn(t, true).Update
	run := func() bool { return runGate("s1", tr, cwd, TriggerStop).Blocked }

	writeUserTurns(t, tr, 5)
	if run() || calls.Load() != 0 {
		t.Fatalf("5 user turns (2 work units) sit under the floor, calls=%d", calls.Load())
	}
	writeUserTurns(t, tr, 6)
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
	calls := &stubSpawn(t, true).Update
	gate := func(trigger string) { runGate("s1", tr, cwd, trigger) }

	writeEdits(t, tr, 3)
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
	writeEdits(t, tr, 20)
	cwd := t.TempDir()
	stubSpawn(t, false)
	runGate("x", tr, cwd, TriggerStop)
}

func TestSessionUpdateUnwritableStampFailsOpen(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(UpdateChildEnv, "")
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	cwd := filepath.Join(t.TempDir(), "proj")
	writeEdits(t, tr, 5)
	stubSpawn(t, false)
	if err := os.MkdirAll(filepath.Join(stampPath("s1"), "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if g := runGate("s1", tr, cwd, TriggerStop); g.Blocked || !g.FailedOpen {
		t.Errorf("an unwritable stamp cannot count blocks, so Stop must fail open; got %+v", g)
	}
}
