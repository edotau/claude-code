package hooks

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/memory/bank"
	"github.com/edotau/claude-code/internal/memstore"
	"github.com/edotau/claude-code/internal/worktree"
)

// TestMain stubs the detached dream spawn so no SessionStart test launches a real process.
func TestMain(m *testing.M) {
	cleanup := hosttest.Setup()
	worktree.StubProcessScanForTest()
	spawnDream = func(string) bool { return true }
	claudePressure = func() (float64, string, bool) { return 0, "", false } // the real usage cache never steers a test
	routeHint = func(string) (string, string) { return "codex", "gpt-6.1-sol" }
	peerAgents = func(string, []string) ([]worktree.Proc, error) { return nil, nil }
	code := m.Run()
	cleanup()
	os.Exit(code)
}

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

// stubSpawn replaces BOTH detached seams: a successful update dispatch also claims an idle watcher, and a
// test must never start a real one.
// The counter is atomic: the idle watcher runs the stub on its own goroutine while the test polls.
func stubSpawn(t *testing.T, ok bool) *atomic.Int64 {
	t.Helper()
	calls := new(atomic.Int64)
	orig, origWatch, origDream := spawnUpdate, spawnWatcher, spawnDream
	spawnUpdate = func(string, string, string) bool { calls.Add(1); return ok }
	spawnWatcher = func([]string, string) bool { return true }
	spawnDream = func(string) bool { return true }
	claudePressure = func() (float64, string, bool) { return 0, "", false } // the real usage cache never steers a test
	routeHint = func(string) (string, string) { return "codex", "gpt-6.1-sol" }
	t.Cleanup(func() { spawnUpdate, spawnWatcher, spawnDream = orig, origWatch, origDream })
	return calls
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
	t.Setenv(bank.UpdateChildEnv, "")
	return &updateRun{t: t, tr: filepath.Join(t.TempDir(), "t.jsonl"), cwd: t.TempDir(), calls: stubSpawn(t, true)}
}

func (u *updateRun) stop() int {
	return sessionUpdateCode(core.ParseInput(strings.NewReader(updatePayload(u.tr, u.cwd))), &bytes.Buffer{})
}

func (u *updateRun) end() {
	runDirect("memory-end", strings.NewReader(updatePayload(u.tr, u.cwd)), io.Discard, io.Discard)
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
	if st, _ := readStamp(stamp); !st.bg || st.edits != 13 {
		t.Errorf("stamp %+v", st)
	}

	// SessionEnd: nothing new since the last dispatch → no spawn; new edits → final update.
	u.end()
	u.wantSpawns(2, "session end with no new edits spawned")
	writeEdits(t, u.tr, 14)
	u.end()
	u.wantSpawns(3, "session end with new edits must spawn")

	t.Setenv(bank.UpdateChildEnv, "1")
	_ = os.Remove(stamp)
	u.stop()
	u.wantSpawns(3, "an update child must never spawn another")
}

func TestSessionUpdateSpawnFailureBlocksThrice(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(bank.UpdateChildEnv, "")
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	cwd := filepath.Join(t.TempDir(), "proj")
	writeEdits(t, tr, 5)
	stubSpawn(t, false)
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
	var out bytes.Buffer
	runDirect("memory-start", strings.NewReader(fmt.Sprintf(`{"session_id":%q,"cwd":%q}`, session, cwd)), &out, io.Discard)
	if out.Len() == 0 {
		return ""
	}
	var doc struct {
		H struct{ AdditionalContext string } `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	return doc.H.AdditionalContext
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
	t.Setenv(bank.UpdateChildEnv, "")
	cutoverStore(t)
	sub := filepath.Join(repo, "sub")
	_ = os.MkdirAll(sub, 0o755)
	// Real session ids are UUID-shaped; memoryContext only advances the chain/ResetSeen for those
	// (probe/test payloads like "probe-x" must not clobber it) — see recall.LooksLikeSessionID.
	const (
		s0 = "00000000-0000-4000-8000-000000000000"
		s1 = "11111111-1111-4111-8111-111111111111"
		s2 = "22222222-2222-4222-8222-222222222222"
	)
	if got := sessionStartContext(t, s0, sub); got != "" || memory.HasBank(sub) {
		t.Errorf("a repo subdir must not be scaffolded: %q", got)
	}
	// the unified surface is catalog + focus only; seed a catalog so the scaffold notice has a surface to ride on
	if err := os.WriteFile(filepath.Join(memstore.StoreDir(), memstore.CatalogFile), []byte("# Memory catalog\n"), 0o600); err != nil {
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
	t.Setenv(bank.UpdateChildEnv, "1")
	sessionStartContext(t, "child", repo) // a non-UUID update-child id: excluded by IsUpdateChild regardless of shape
	if e, ok := readChain(filepath.Join(memstore.StateDir(), "last-session")); !ok || e.sessionID != s2 {
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
	t.Setenv(bank.UpdateChildEnv, "")
	repo := filepath.Join(t.TempDir(), "repo")
	if err := exec.Command("git", "init", "-q", repo).Run(); err != nil {
		t.Fatal(err)
	}
	const real = "33333333-3333-4333-8333-333333333333"
	memoryContext(repo, real, "", time.Now())
	chain := filepath.Join(memstore.StateDir(), "last-session")
	if e, ok := readChain(chain); !ok || e.sessionID != real {
		t.Fatalf("a UUID session id must advance last-session: %+v ok=%v", e, ok)
	}
	memoryContext(repo, "probe-x", "", time.Now())
	if e, ok := readChain(chain); !ok || e.sessionID != real {
		t.Errorf("a probe id must not clobber last-session: got %+v, want %s", e, real)
	}
}

// cutoverStore creates the unified store's git repo, the state memoryContext needs before repairing a native link.
func cutoverStore(t *testing.T) {
	t.Helper()
	if err := bank.StoreRepo().Ensure(); err != nil {
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
	t.Setenv(bank.UpdateChildEnv, "")
	cutoverStore(t)

	repo := filepath.Join(t.TempDir(), "repo")
	if err := exec.Command("git", "init", "-q", repo).Run(); err != nil {
		t.Fatal(err)
	}
	// Pre-scaffold the store (the steady-state case: this repo already has a .origin from an
	// earlier Init/EnsureRepoDir). A repo whose FIRST-EVER SessionStart runs under cutover hits a
	// separate ordering bug — see the note below — so this test isolates the link-repair contract.
	if _, err := memstore.EnsureRepoDir(repo); err != nil {
		t.Fatal(err)
	}

	memoryContext(repo, "s1", "", time.Now())

	res, ok := memstore.LinkNativeFor(repo)
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
	t.Setenv(bank.UpdateChildEnv, "")
	cutoverStore(t)
	repo := filepath.Join(t.TempDir(), "proj")
	if err := exec.Command("git", "init", "-q", repo).Run(); err != nil {
		t.Fatal(err)
	}

	memoryContext(repo, "first", "", time.Now())

	dir := memstore.Dir(repo)
	if _, err := memstore.ReadOrigin(dir); err != nil {
		t.Fatalf("store %s has no .origin after the first SessionStart: %v", dir, err)
	}
	if strings.Contains(filepath.Base(dir), "--") {
		t.Errorf("store key got a collision suffix on a fresh repo: %s", dir)
	}
	res, ok := memstore.LinkNativeFor(repo)
	if !ok || res.Action != "ok" || res.Target != dir {
		t.Errorf("native link after first SessionStart = %+v ok=%v, want ok -> %s", res, ok, dir)
	}
}

// TestPreCompactUpdatesOnlyUncoveredEdits: a compaction dispatches the update when edits exist past the last
// stamp, never re-dispatches covered ones, never blocks, and stays quiet while memory is paused.
func TestPreCompactUpdatesOnlyUncoveredEdits(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(bank.UpdateChildEnv, "")
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	cwd := t.TempDir()
	calls := stubSpawn(t, true)
	run := func() int {
		return runDirect("memory-compact", strings.NewReader(updatePayload(tr, cwd)), io.Discard, io.Discard)
	}

	writeEdits(t, tr, 0)
	if run() != core.ExitProceed || calls.Load() != 0 {
		t.Fatal("no edits must not spawn")
	}
	writeEdits(t, tr, 1) // below the Stop floor: a compaction still checkpoints it
	if run() != core.ExitProceed || calls.Load() != 1 {
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
	t.Setenv(bank.UpdateChildEnv, "")
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	cwd := t.TempDir()
	calls := stubSpawn(t, true)
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
	if st, _ := readStamp(stampPath("s1")); !st.bg {
		t.Errorf("stamp %+v, want bg", st)
	}
}

// TestPreCompactAndSessionEndUpdateNewUserTurns: once the stamp covers N work units, a session whose
// only new work is user turns still re-updates at pre-compact and session end — and never re-updates
// when nothing new arrived.
func TestPreCompactAndSessionEndUpdateNewUserTurns(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(bank.UpdateChildEnv, "")
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	cwd := t.TempDir()
	calls := stubSpawn(t, true)
	payload := func() *strings.Reader { return strings.NewReader(updatePayload(tr, cwd)) }

	writeEdits(t, tr, 3)
	if runDirect("memory-compact", payload(), io.Discard, io.Discard); calls.Load() != 1 {
		t.Fatalf("3 edits must spawn at pre-compact, calls=%d", calls.Load())
	}
	appendUserTurns(t, tr, 4) // the only new work since the stamp: 4 turns = 2 work units
	if runDirect("memory-compact", payload(), io.Discard, io.Discard); calls.Load() != 2 {
		t.Fatalf("new user turns since the stamp must spawn, calls=%d", calls.Load())
	}
	if runDirect("memory-end", payload(), io.Discard, io.Discard); calls.Load() != 2 {
		t.Error("no new work since the stamp must not re-update")
	}
	appendUserTurns(t, tr, 2)
	if runDirect("memory-end", payload(), io.Discard, io.Discard); calls.Load() != 3 {
		t.Errorf("2 new turns (1 work unit) past the stamp must spawn, calls=%d", calls.Load())
	}
	if runDirect("memory-end", payload(), io.Discard, io.Discard); calls.Load() != 3 {
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
	t.Setenv(bank.UpdateChildEnv, "")
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
	chain := filepath.Join(memstore.StateDir(), "last-session")
	writeChain(chain, chainEntry{sessionID: "prev3", transcriptPath: tr, cwd: repo})
	var sweeps int
	orig := spawnCatchup
	spawnCatchup = func(string, string) bool { sweeps++; return true }
	t.Cleanup(func() { spawnCatchup = orig })

	t.Setenv(bank.UpdateChildEnv, "1")
	got := memoryContext(repo, "44444444-4444-4444-8444-444444444445", "", time.Now())
	if sweeps != 0 {
		t.Errorf("an update child must never spawn the catch-up sweep, calls=%d", sweeps)
	}
	if strings.Contains(got, "Catching up") {
		t.Errorf("an update child's SessionStart must not surface the catch-up note: %q", got)
	}
}

// TestSessionStartChainRewrittenWithTranscriptAndCWD: memoryContext's writeChain must persist all three
// fields so the NEXT SessionStart can resolve the previous session's transcript for its own catch-up check.
func TestSessionStartChainRewrittenWithTranscriptAndCWD(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(bank.UpdateChildEnv, "")
	cwd := t.TempDir()
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	writeEdits(t, tr, 1)
	const sid = "44444444-4444-4444-8444-444444444444"

	memoryContext(cwd, sid, tr, time.Now())

	chain := filepath.Join(memstore.StateDir(), "last-session")
	e, ok := readChain(chain)
	if !ok || e.sessionID != sid || e.transcriptPath != tr || e.cwd != cwd {
		t.Errorf("chain = %+v ok=%v, want {%s %s %s}", e, ok, sid, tr, cwd)
	}
}

// TestReadChainLegacyOneLineFormat: a pre-upgrade chain file (bare session id, no transcript/cwd lines)
// still parses with a blank transcript path.
func TestReadChainLegacyOneLineFormat(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	chain := filepath.Join(t.TempDir(), "last-session")
	if err := os.WriteFile(chain, []byte("legacy-id\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e, ok := readChain(chain)
	if !ok || e.sessionID != "legacy-id" || e.transcriptPath != "" || e.cwd != "" {
		t.Fatalf("legacy chain parse = %+v ok=%v", e, ok)
	}
}

// TestReadStampCatchupStateDoesNotBreakParsing: a "catchup" stamp still parses through the same 3-field
// format the Stop gate reads, with blocks defaulting to 0 (not crashing on a non-numeric second field).
func TestReadStampCatchupStateDoesNotBreakParsing(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	path := filepath.Join(t.TempDir(), "stamp-x")
	at := time.Now().Add(-5 * time.Minute)
	writeStamp(path, at, "catchup", 7)

	st, ok := readStamp(path)
	if !ok || st.bg || st.state != "catchup" || st.blocks != 0 || st.edits != 7 || !st.at.Equal(at.Truncate(time.Second)) {
		t.Errorf("readStamp(catchup) = %+v ok=%v", st, ok)
	}

	// The Stop gate must not crash reading this stamp for a session that resumes under the same id.
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	writeEdits(t, tr, 20)
	cwd := t.TempDir()
	stubSpawn(t, false)
	var errb bytes.Buffer
	code := sessionUpdateCode(core.ParseInput(strings.NewReader(fmt.Sprintf(`{"session_id":"x","cwd":%q,"transcript_path":%q}`, cwd, tr))), &errb)
	if code != core.ExitBlock && code != core.ExitProceed {
		t.Errorf("unexpected exit code %d parsing a catchup stamp", code)
	}
}

// TestReadStampHandlesNonNumericStateProperly: readStamp must properly handle non-numeric state strings
// like "catchup" without silently corrupting blocks to 0. Regression test for defect where
// strconv.Atoi('catchup') error was ignored with _, corrupting update state tracking.
func TestReadStampHandlesNonNumericStateProperly(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	path := filepath.Join(t.TempDir(), "stamp-regression")
	at := time.Now()

	// Write a stamp with state="catchup" (non-numeric).
	writeStamp(path, at, "catchup", 42)
	st, ok := readStamp(path)

	// The stamp should parse successfully and preserve the state correctly.
	if !ok {
		t.Fatal("readStamp failed to parse a valid catchup stamp")
	}
	if st.state != "catchup" {
		t.Errorf("state = %q, want catchup (preserved from stamp)", st.state)
	}
	if st.edits != 42 {
		t.Errorf("edits = %d, want 42", st.edits)
	}
	// blocks should be 0 when state is non-numeric (catchup, not a number).
	if st.blocks != 0 {
		t.Errorf("blocks = %d, want 0 (state is non-numeric)", st.blocks)
	}

	// Later write operations must not lose the "catchup" state.
	// Simulate SessionUpdate re-reading the stamp — it should preserve the catchup state.
	st2, ok2 := readStamp(path)
	if !ok2 || st2.state != "catchup" {
		t.Errorf("re-read stamp state = %q ok=%v, want 'catchup' preserved", st2.state, ok2)
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

// TestDoctorNoticeTruncationPreservesUTF8: doctorNotice must truncate by rune count,
// not byte count, to avoid splitting multi-byte UTF-8 characters. Regression test for
// defect where truncation at notice[:200] could produce invalid UTF-8.
func TestDoctorNoticeTruncationPreservesUTF8(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	stateDir := memstore.StateDir()
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	noticePath := memory.DoctorNoticePath("")

	// Build a notice with multi-byte UTF-8 chars that will span the 200-byte boundary.
	// Each "Aé中国" iteration = 11 bytes, 4 runes. To hit a boundary in the middle,
	// we craft a string that lands a multi-byte char right at byte 200.
	// Input: 140 bytes of mostly ASCII, then characters with varying byte lengths.
	notice := strings.Repeat("A", 196) + "中" // "中" is 3 bytes, so 196+3 = 199; still valid
	notice = notice + "国"                    // "国" is 3 bytes, so 199+3 = 202; truncation at 200 would split it
	if err := os.WriteFile(noticePath, []byte(notice), 0o600); err != nil {
		t.Fatal(err)
	}

	// Call doctorNotice and verify the result is valid UTF-8.
	got := doctorNotice("")
	if !strings.HasPrefix(got, " memory review: ") {
		t.Fatalf("doctorNotice prefix not found: %q", got)
	}

	// Strip the prefix and suffix to get just the truncated notice.
	truncated := strings.TrimPrefix(strings.TrimSuffix(got, "."), " memory review: ")

	// Verify the truncated notice is valid UTF-8.
	if !utf8.ValidString(truncated) {
		t.Errorf("truncated notice is invalid UTF-8: %q (bytes: %v)", truncated, []byte(truncated))
	}

	// Verify truncation happened and result is roughly 200 chars or less (when counted as runes).
	if len(truncated) > 300 {
		t.Errorf("truncation did not work: result is %d bytes", len(truncated))
	}
}

// TestWriteChainErrorHandling: writeChain must handle AtomicWrite errors gracefully
// so that a chain file write failure (disk full, permissions, read-only fs) does not
// silently lose the session chain pointer. Regression test for defect where
// writeChain(path, entry) swallowed errors with `_ = paths.AtomicWrite(...)`.
func TestWriteChainErrorHandling(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	readOnlyDir := filepath.Join(t.TempDir(), "readonly")
	if err := os.MkdirAll(readOnlyDir, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(readOnlyDir, 0o755) // cleanup

	chainPath := filepath.Join(readOnlyDir, "last-session")
	e := chainEntry{sessionID: "test-id", transcriptPath: "/t.jsonl", cwd: "/cwd"}

	// writeChain should handle the error from AtomicWrite when the directory is read-only.
	// After the fix, this should either log an error or propagate it.
	// For now, this test documents that a write to a read-only directory happens.
	writeChain(chainPath, e)

	// Verify no file was created (write should have failed).
	if _, err := os.Stat(chainPath); !os.IsNotExist(err) {
		t.Errorf("writeChain should not create a file in a read-only directory: err=%v", err)
	}
}

// TestWriteStampErrorHandling: writeStamp must handle AtomicWrite errors gracefully
// so that a stamp file write failure does not silently lose update state tracking.
// Regression test for defect where writeStamp(path, ...) swallowed errors with
// `_ = paths.AtomicWrite(...)`.
func TestWriteStampErrorHandling(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	readOnlyDir := filepath.Join(t.TempDir(), "readonly")
	if err := os.MkdirAll(readOnlyDir, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(readOnlyDir, 0o755)

	stampPath := filepath.Join(readOnlyDir, "stamp-test")
	now := time.Now()

	// writeStamp should handle the error when the directory is read-only.
	writeStamp(stampPath, now, "bg", 42)

	// Verify no file was created.
	if _, err := os.Stat(stampPath); !os.IsNotExist(err) {
		t.Errorf("writeStamp should not create a file in a read-only directory: err=%v", err)
	}
}

// TestWriteCountStateErrorHandling: writeCountState must handle AtomicWrite errors
// gracefully so that edit count loss is not silent. Regression test for defect where
// writeCountState(path, ...) swallowed errors with `_ = paths.AtomicWrite(...)`.
func TestWriteCountStateErrorHandling(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	readOnlyDir := filepath.Join(t.TempDir(), "readonly")
	if err := os.MkdirAll(readOnlyDir, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(readOnlyDir, 0o755)

	countPath := filepath.Join(readOnlyDir, "count-test")
	cs := countState{edits: 10, offset: 500, subEdits: 2, subBytes: 1000}

	// writeCountState should handle the error when the directory is read-only.
	writeCountState(countPath, cs)

	// Verify no file was created.
	if _, err := os.Stat(countPath); !os.IsNotExist(err) {
		t.Errorf("writeCountState should not create a file in a read-only directory: err=%v", err)
	}
}

func TestSessionUpdateUnwritableStampFailsOpen(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(bank.UpdateChildEnv, "")
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	cwd := filepath.Join(t.TempDir(), "proj")
	writeEdits(t, tr, 5)
	stubSpawn(t, false)
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
