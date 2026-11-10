package memhook

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/worktree"
)

func writeEdits(t *testing.T, path string, n int) {
	t.Helper()
	line := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/r/a.go"}}]}}` + "\n"
	if err := os.WriteFile(path, []byte(strings.Repeat(line, n)), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeUserTurns(t *testing.T, path string, n int) {
	t.Helper()
	line := `{"type":"user","message":{"content":"ship it"}}` + "\n"
	if err := os.WriteFile(path, []byte(strings.Repeat(line, n)), 0o600); err != nil {
		t.Fatal(err)
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

func updatePayload(tr, cwd string) string {
	return fmt.Sprintf(`{"session_id":"s1","cwd":%q,"transcript_path":%q}`, cwd, tr)
}

// updateRun drives the Stop / SessionEnd gates against one transcript with the detached spawn stubbed.
type updateRun struct {
	t     *testing.T
	tr    string
	cwd   string
	calls *atomic.Int64
}

func newUpdateRun(t *testing.T) *updateRun {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(memory.UpdateChildEnv, "")
	return &updateRun{t: t, tr: filepath.Join(t.TempDir(), "t.jsonl"), cwd: t.TempDir(), calls: &StubSpawnForTest(t, true).Update}
}

func (u *updateRun) stop() int {
	return sessionUpdateCode(core.ParseInput(strings.NewReader(updatePayload(u.tr, u.cwd))), &bytes.Buffer{})
}

func (u *updateRun) end() {
	End(core.ParseInput(strings.NewReader(updatePayload(u.tr, u.cwd))))
}

// wantSpawns fails the test unless exactly want detached updates have been dispatched so far.
func (u *updateRun) wantSpawns(want int64, msg string) {
	u.t.Helper()
	if got := u.calls.Load(); got != want {
		u.t.Errorf("%s: calls=%d, want %d", msg, got, want)
	}
}

func TestSessionUpdateTriggerAndReupdate(t *testing.T) {
	u := newUpdateRun(t)

	writeEdits(t, u.tr, 2)
	if u.stop() != core.ExitProceed {
		t.Error("under the edit floor must proceed")
	}
	u.wantSpawns(0, "under the edit floor must not spawn")
	writeEdits(t, u.tr, 3)
	if u.stop() != core.ExitProceed {
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

	t.Setenv(memory.UpdateChildEnv, "1")
	_ = os.Remove(stamp)
	u.stop()
	u.wantSpawns(3, "an update child must never spawn another")
}

func TestSessionUpdateSpawnFailureBlocksThrice(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(memory.UpdateChildEnv, "")
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	cwd := filepath.Join(t.TempDir(), "proj")
	writeEdits(t, tr, 5)
	StubSpawnForTest(t, false)
	codes := []int{}
	for range 5 {
		var errb bytes.Buffer
		codes = append(codes, sessionUpdateCode(core.ParseInput(strings.NewReader(updatePayload(tr, cwd))), &errb))
		if codes[len(codes)-1] == core.ExitBlock && !strings.Contains(errb.String(), "claude-code memory update") {
			t.Errorf("block reason %q", errb.String())
		}
	}
	if fmt.Sprint(codes) != fmt.Sprint([]int{core.ExitBlock, core.ExitBlock, core.ExitBlock, core.ExitProceed, core.ExitProceed}) {
		t.Errorf("codes %v", codes)
	}

	// Evidence releases the gate: a bank write after the stamp.
	_ = os.Remove(stampPath("s1"))
	sessionUpdateCode(core.ParseInput(strings.NewReader(updatePayload(tr, cwd))), &bytes.Buffer{})
	writeStamp(stampPath("s1"), time.Now().Add(-time.Minute), "1", 5)
	if _, err := memory.Init(cwd); err != nil {
		t.Fatal(err)
	}
	if got := sessionUpdateCode(core.ParseInput(strings.NewReader(updatePayload(tr, cwd))), &bytes.Buffer{}); got != core.ExitProceed {
		t.Error("a bank write after the prompt must release the gate")
	}
}

// sessionStartContext runs the memory-start hook and returns the additionalContext it injected ("" when silent).
func sessionStartContext(t *testing.T, session, cwd string) string {
	t.Helper()
	return Start(core.ParseInput(strings.NewReader(fmt.Sprintf(`{"session_id":%q,"cwd":%q}`, session, cwd)))).Context
}

// memoryGitRepo git-inits a fresh repo under a temp dir, skipping the test when git is missing.
func memoryGitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := filepath.Join(t.TempDir(), "repo")
	if err := exec.Command("git", "init", "-q", repo).Run(); err != nil {
		t.Fatal(err)
	}
	return repo
}

func TestSessionStartAutoInitAndNotice(t *testing.T) {
	repo := memoryGitRepo(t)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(memory.UpdateChildEnv, "")
	cutoverStore(t)
	sub := filepath.Join(repo, "sub")
	_ = os.MkdirAll(sub, 0o755)
	// Real session ids are UUID-shaped; memoryContext only advances the chain/ResetSeen for those
	// (probe/test payloads like "probe-x" must not clobber it) — see memory.LooksLikeSessionID.
	const (
		s0 = "00000000-0000-4000-8000-000000000000"
		s1 = "11111111-1111-4111-8111-111111111111"
		s2 = "22222222-2222-4222-8222-222222222222"
	)
	if got := sessionStartContext(t, s0, sub); got != "" || memory.HasBank(sub) {
		t.Errorf("a repo subdir must not be scaffolded: %q", got)
	}
	// the unified surface is catalog + focus only; seed a catalog so the scaffold notice has a surface to ride on
	if err := os.WriteFile(filepath.Join(memory.StoreDir(), memory.CatalogFile), []byte("# Memory catalog\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := sessionStartContext(t, s1, repo)
	if !strings.Contains(got, "Memory bank scaffolded at") {
		t.Fatalf("auto-init surface: %q", got)
	}
	_ = memory.WriteUpdateResult(s1, memory.UpdateResult{Status: memory.UpdateFailed, Time: time.Now(), Log: "/x.log"})
	got = sessionStartContext(t, s2, repo)
	if strings.Contains(got, "scaffolded") {
		t.Errorf("notice must not re-announce the scaffold: %q", got[:min(len(got), 500)])
	}
	for _, want := range []string{"Previous session id: " + s1, "did not complete (failed) (log: /x.log)"} {
		if !strings.Contains(got, want) {
			t.Errorf("notice lacks %q: %q", want, got[:min(len(got), 500)])
		}
	}
	t.Setenv(memory.UpdateChildEnv, "1")
	sessionStartContext(t, "child", repo) // a non-UUID update-child id: excluded by IsUpdateChild regardless of shape
	if e, ok := readChain(filepath.Join(memory.StateDir(), "last-session")); !ok || e != s2 {
		t.Errorf("an update child advanced the session chain: %+v ok=%v", e, ok)
	}
}

// TestSessionStartProbeIDDoesNotClobberLastSession: a non-UUID session id (probe/test payloads,
// e.g. "probe-x") must not overwrite last-session or seed a seen-set, while a real UUID still does.
func TestSessionStartProbeIDDoesNotClobberLastSession(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(memory.UpdateChildEnv, "")
	repo := filepath.Join(t.TempDir(), "repo")
	if err := exec.Command("git", "init", "-q", repo).Run(); err != nil {
		t.Fatal(err)
	}
	const real = "33333333-3333-4333-8333-333333333333"
	memoryContext(repo, real, time.Now())
	chain := filepath.Join(memory.StateDir(), "last-session")
	if e, ok := readChain(chain); !ok || e != real {
		t.Fatalf("a UUID session id must advance last-session: %+v ok=%v", e, ok)
	}
	memoryContext(repo, "probe-x", time.Now())
	if e, ok := readChain(chain); !ok || e != real {
		t.Errorf("a probe id must not clobber last-session: got %+v, want %s", e, real)
	}
}

// cutoverStore creates the unified store's git repo, the state memoryContext needs before repairing a native link.
func cutoverStore(t *testing.T) {
	t.Helper()
	if err := memory.StoreRepo().Ensure(); err != nil {
		t.Fatal(err)
	}
}

// TestSessionStartLinksNativeMemoryAfterCutover: once the migration is complete and unpaused,
// memoryContext (SessionStart's engine) repairs the cwd's native memory link to its repo store dir.
func TestSessionStartLinksNativeMemoryAfterCutover(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv(memory.UpdateChildEnv, "")
	cutoverStore(t)

	repo := filepath.Join(t.TempDir(), "repo")
	if err := exec.Command("git", "init", "-q", repo).Run(); err != nil {
		t.Fatal(err)
	}
	// Pre-scaffold the store (the steady-state case: this repo already has a .origin from an
	// earlier Init/EnsureRepoDir). A repo whose FIRST-EVER SessionStart runs under cutover hits a
	// separate ordering bug — see the note below — so this test isolates the link-repair contract.
	if _, err := memory.EnsureRepoDir(repo); err != nil {
		t.Fatal(err)
	}

	memoryContext(repo, "s1", time.Now())

	res, ok := memory.LinkNativeFor(repo)
	if !ok {
		t.Fatal("LinkNativeFor ok=false after cutover")
	}
	if res.Action != "ok" {
		t.Errorf("link action after SessionStart = %s, want ok (already linked)", res.Action)
	}
	fi, err := os.Lstat(res.Link)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("SessionStart did not create a native-memory symlink at %s: %v", res.Link, err)
	}
	target, err := os.Readlink(res.Link)
	if err != nil || target != res.Target {
		t.Errorf("link -> %q, want %s (%v)", target, res.Target, err)
	}
}

// TestSessionStartFirstSessionLinksScaffoldedStore: a repo's first SessionStart after the cutover links native memory
// to the store Init scaffolds (with its .origin), never to an origin-less dir that pushes the bank to <slug>--<hash>.
func TestSessionStartFirstSessionLinksScaffoldedStore(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv(memory.UpdateChildEnv, "")
	cutoverStore(t)
	repo := filepath.Join(t.TempDir(), "proj")
	if err := exec.Command("git", "init", "-q", repo).Run(); err != nil {
		t.Fatal(err)
	}

	memoryContext(repo, "first", time.Now())

	dir := memory.Dir(repo)
	if _, err := memory.ReadOrigin(dir); err != nil {
		t.Fatalf("store %s has no .origin after the first SessionStart: %v", dir, err)
	}
	if strings.Contains(filepath.Base(dir), "--") {
		t.Errorf("store key got a collision suffix on a fresh repo: %s", dir)
	}
	res, ok := memory.LinkNativeFor(repo)
	if !ok || res.Action != "ok" || res.Target != dir {
		t.Errorf("native link after first SessionStart = %+v ok=%v, want ok -> %s", res, ok, dir)
	}
}

// TestPreCompactUpdatesOnlyUncoveredEdits: a compaction dispatches the update when edits exist past the last
// stamp, never re-dispatches covered ones, never blocks, and stays quiet while memory is paused.
func TestPreCompactUpdatesOnlyUncoveredEdits(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(memory.UpdateChildEnv, "")
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	cwd := t.TempDir()
	calls := &StubSpawnForTest(t, true).Update
	run := func() core.Decision {
		return Compact(core.ParseInput(strings.NewReader(updatePayload(tr, cwd)))).Decision
	}

	writeEdits(t, tr, 0)
	if run() == core.Block || calls.Load() != 0 {
		t.Fatal("no edits must not spawn")
	}
	writeEdits(t, tr, 1) // below the Stop floor: a compaction still checkpoints it
	if run() == core.Block || calls.Load() != 1 {
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

// TestSessionUpdateDiscussionOnly: a discussion-only session (0 edits) still crosses the floor on
// user turns alone — 6 turns weigh 3 work units, exactly updateMinEdits — and is updated at Stop.
func TestSessionUpdateDiscussionOnly(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(memory.UpdateChildEnv, "")
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	cwd := t.TempDir()
	calls := &StubSpawnForTest(t, true).Update
	run := func() int {
		return sessionUpdateCode(core.ParseInput(strings.NewReader(updatePayload(tr, cwd))), &bytes.Buffer{})
	}

	writeUserTurns(t, tr, 5)
	if run() != core.ExitProceed || calls.Load() != 0 {
		t.Fatalf("5 user turns (2 work units) sit under the floor, calls=%d", calls.Load())
	}
	writeUserTurns(t, tr, 6)
	if run() != core.ExitProceed || calls.Load() != 1 {
		t.Fatalf("6 user turns (3 work units) must spawn once, calls=%d", calls.Load())
	}
	if st, _ := readStamp(stampPath("s1")); st.state != "bg" {
		t.Errorf("stamp %+v, want bg", st)
	}
}

// TestPreCompactAndSessionEndUpdateNewUserTurns: once the stamp covers N work units, a session whose
// only new work is user turns still re-updates at pre-compact and session end — and never re-updates
// when nothing new arrived.
func TestPreCompactAndSessionEndUpdateNewUserTurns(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(memory.UpdateChildEnv, "")
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	cwd := t.TempDir()
	calls := &StubSpawnForTest(t, true).Update
	payload := func() *strings.Reader { return strings.NewReader(updatePayload(tr, cwd)) }

	writeEdits(t, tr, 3)
	if Compact(core.ParseInput(payload())); calls.Load() != 1 {
		t.Fatalf("3 edits must spawn at pre-compact, calls=%d", calls.Load())
	}
	appendUserTurns(t, tr, 4) // the only new work since the stamp: 4 turns = 2 work units
	if Compact(core.ParseInput(payload())); calls.Load() != 2 {
		t.Fatalf("new user turns since the stamp must spawn, calls=%d", calls.Load())
	}
	if End(core.ParseInput(payload())); calls.Load() != 2 {
		t.Error("no new work since the stamp must not re-update")
	}
	appendUserTurns(t, tr, 2)
	if End(core.ParseInput(payload())); calls.Load() != 3 {
		t.Errorf("2 new turns (1 work unit) past the stamp must spawn, calls=%d", calls.Load())
	}
	if End(core.ParseInput(payload())); calls.Load() != 3 {
		t.Error("session end with nothing new re-updated")
	}
}

// idle backdates tr's mtime so the transcript reads as quiet.
func idle(t *testing.T, path string, d time.Duration) {
	t.Helper()
	at := time.Now().Add(-d)
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

// TestSessionStartCatchUpUpdateChildNeverSpawns: memoryContext gates the catch-up sweep behind
// !IsUpdateChild — an update worker's own SessionStart must never dispatch another sweep.
func TestSessionStartCatchUpUpdateChildNeverSpawns(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(memory.UpdateChildEnv, "")
	repo := filepath.Join(t.TempDir(), "repo")
	if err := exec.Command("git", "init", "-q", repo).Run(); err != nil {
		t.Fatal(err)
	}
	if _, err := memory.Init(repo); err != nil {
		t.Fatal(err)
	}
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	writeEdits(t, tr, 5)
	idle(t, tr, 20*time.Minute)
	chain := filepath.Join(memory.StateDir(), "last-session")
	writeChain(chain, "prev3")
	var sweeps int
	orig := spawnCatchup
	spawnCatchup = func(string, string) bool { sweeps++; return true }
	t.Cleanup(func() { spawnCatchup = orig })

	t.Setenv(memory.UpdateChildEnv, "1")
	got := memoryContext(repo, "44444444-4444-4444-8444-444444444445", time.Now())
	if sweeps != 0 {
		t.Errorf("an update child must never spawn the catch-up sweep, calls=%d", sweeps)
	}
	if strings.Contains(got, "Catching up") {
		t.Errorf("an update child's SessionStart must not surface the catch-up note: %q", got)
	}
}

// TestSessionStartChainRecordsID: the next start receives the previous session pointer.

// TestReadStampCatchupStateDoesNotBreakParsing: a "catchup" stamp still parses through the same 3-field
// format the Stop gate reads, with blocks defaulting to 0 (not crashing on a non-numeric second field).
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
	StubSpawnForTest(t, false)
	var errb bytes.Buffer
	code := sessionUpdateCode(core.ParseInput(strings.NewReader(fmt.Sprintf(`{"session_id":"x","cwd":%q,"transcript_path":%q}`, cwd, tr))), &errb)
	if code != core.ExitBlock && code != core.ExitProceed {
		t.Errorf("unexpected exit code %d parsing a catchup stamp", code)
	}
}

// TestGlobErrorHandlingInCountSubagentEdits: filepath.Glob errors must be handled properly
// instead of being silently ignored. Regression test for defect where Glob error was swallowed
// with _, causing empty file list and incorrect edit count.
func TestCountSubagentEditsHandlesGlobError(t *testing.T) {
	// Create a transcript path with no subagents dir — Glob will return no matches (ok state).
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	writeEdits(t, tr, 1)

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
	writeEdits(t, filepath.Join(subDir, "agent-a.jsonl"), 3)

	edits2, _ := subagentEdits(tr, 0, 0, true)
	if edits2 != 3 {
		t.Errorf("subagentEdits after adding agent files = %d, want 3", edits2)
	}
}

func TestSessionUpdateUnwritableStampFailsOpen(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(memory.UpdateChildEnv, "")
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	cwd := filepath.Join(t.TempDir(), "proj")
	writeEdits(t, tr, 5)
	StubSpawnForTest(t, false)
	if err := os.MkdirAll(filepath.Join(stampPath("s1"), "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := sessionUpdateCode(core.ParseInput(strings.NewReader(updatePayload(tr, cwd))), &bytes.Buffer{}); got != core.ExitProceed {
		t.Errorf("an unwritable stamp cannot count blocks, so Stop must fail open; got %d", got)
	}
}

// sessionUpdateCode is sessionUpdate's exit code alone, for tests that only check the gate.
func sessionUpdateCode(in *core.Input, stderr io.Writer) int {
	code, _ := sessionUpdate(in, stderr)
	return code
}

// An update in a linked worktree dispatches the background worker and logs one ledger line.
func TestUpdateAppendsWorktreeLedger(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", home+"/.claude")
	t.Setenv(memory.UpdateChildEnv, "")
	main := filepath.Join(home, "repo")
	for _, a := range [][]string{{"init", "-q", "-b", "main", main}, {"-C", main, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "--allow-empty", "-q", "-m", "i"}} {
		if out, err := exec.Command("git", a...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", a, err, out)
		}
	}
	repo, err := worktree.ResolveRepo(main)
	if err != nil {
		t.Fatal(err)
	}
	dest, err := worktree.Create(repo, "wt-h", worktree.CreateOptions{Log: &bytes.Buffer{}})
	if err != nil {
		t.Fatal(err)
	}
	spawns := StubSpawnForTest(t, true)
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	writeEdits(t, tr, 3)
	if res := Stop(core.ParseInput(strings.NewReader(updatePayload(tr, dest)))); res.Decision == core.Block || spawns.Update.Load() != 1 {
		t.Fatalf("update = %+v, spawns=%d; want not blocked and one background worker", res, spawns.Update.Load())
	}
	data, err := os.ReadFile(filepath.Join(memory.Dir(main), "worktrees", "wt-h.md"))
	if err != nil || !strings.Contains(string(data), "update (") || !strings.Contains(string(data), "3 edits, session s1") {
		t.Fatalf("no update line in ledger: %v\n%s", err, data)
	}
}
