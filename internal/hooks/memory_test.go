package hooks

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/memory"
)

// TestMain stubs the detached dream spawn so no SessionStart test launches a real process.
func TestMain(m *testing.M) {
	spawnDream = func(string) bool { return true }
	os.Exit(m.Run())
}

func writeEdits(t *testing.T, path string, n int) {
	t.Helper()
	line := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/r/a.go"}}]}}` + "\n"
	if err := os.WriteFile(path, []byte(strings.Repeat(line, n)), 0o600); err != nil {
		t.Fatal(err)
	}
}

// stubSpawn replaces BOTH detached seams: a successful harvest dispatch also claims an idle watcher, and a
// test must never start a real one.
// The counter is atomic: the idle watcher runs the stub on its own goroutine while the test polls.
func stubSpawn(t *testing.T, ok bool) *atomic.Int64 {
	t.Helper()
	calls := new(atomic.Int64)
	orig, origWatch, origDream := spawnHarvest, spawnWatcher, spawnDream
	spawnHarvest = func(string, string, string) bool { calls.Add(1); return ok }
	spawnWatcher = func([]string, string) bool { return true }
	spawnDream = func(string) bool { return true }
	t.Cleanup(func() { spawnHarvest, spawnWatcher, spawnDream = orig, origWatch, origDream })
	return calls
}

func harvestPayload(tr, cwd string) string {
	return fmt.Sprintf(`{"session_id":"s1","cwd":%q,"transcript_path":%q}`, cwd, tr)
}

func TestSessionHarvestTriggerAndReharvest(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(memory.HarvestChildEnv, "")
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	cwd := t.TempDir()
	calls := stubSpawn(t, true)
	run := func() int { return SessionHarvest(strings.NewReader(harvestPayload(tr, cwd)), &bytes.Buffer{}) }

	writeEdits(t, tr, 2)
	if run() != ExitProceed || calls.Load() != 0 {
		t.Fatal("under the edit floor must not spawn")
	}
	writeEdits(t, tr, 3)
	if run() != ExitProceed || calls.Load() != 1 {
		t.Fatalf("3 edits must spawn once, calls=%d", calls.Load())
	}
	writeEdits(t, tr, 20)
	if run(); calls.Load() != 1 {
		t.Error("re-harvest inside the cooldown")
	}
	stamp := stampPath("s1")
	writeStamp(stamp, time.Now().Add(-16*time.Minute), "bg", 3)
	writeEdits(t, tr, 12)
	if run(); calls.Load() != 1 {
		t.Error("re-harvest with fewer than 10 new edits")
	}
	writeEdits(t, tr, 13)
	if run(); calls.Load() != 2 {
		t.Error("10 new edits after the cooldown must re-harvest")
	}
	if st, _ := readStamp(stamp); !st.bg || st.edits != 13 {
		t.Errorf("stamp %+v", st)
	}

	// SessionEnd: nothing new since the last dispatch → no spawn; new edits → final harvest.
	if SessionHarvestEnd(strings.NewReader(harvestPayload(tr, cwd))); calls.Load() != 2 {
		t.Error("session end with no new edits spawned")
	}
	writeEdits(t, tr, 14)
	if SessionHarvestEnd(strings.NewReader(harvestPayload(tr, cwd))); calls.Load() != 3 {
		t.Error("session end with new edits must spawn")
	}

	t.Setenv(memory.HarvestChildEnv, "1")
	_ = os.Remove(stamp)
	if run(); calls.Load() != 3 {
		t.Error("a harvest child must never spawn another")
	}
}

func TestSessionHarvestSpawnFailureBlocksThrice(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(memory.HarvestChildEnv, "")
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	cwd := filepath.Join(t.TempDir(), "proj")
	writeEdits(t, tr, 5)
	stubSpawn(t, false)
	codes := []int{}
	for range 5 {
		var errb bytes.Buffer
		codes = append(codes, SessionHarvest(strings.NewReader(harvestPayload(tr, cwd)), &errb))
		if codes[len(codes)-1] == ExitBlock && !strings.Contains(errb.String(), "/reflection:update") {
			t.Errorf("block reason %q", errb.String())
		}
	}
	if fmt.Sprint(codes) != fmt.Sprint([]int{ExitBlock, ExitBlock, ExitBlock, ExitProceed, ExitProceed}) {
		t.Errorf("codes %v", codes)
	}

	// Evidence releases the gate: a bank write after the stamp.
	_ = os.Remove(stampPath("s1"))
	SessionHarvest(strings.NewReader(harvestPayload(tr, cwd)), &bytes.Buffer{})
	writeStamp(stampPath("s1"), time.Now().Add(-time.Minute), "1", 5)
	if _, err := memory.Init(cwd); err != nil {
		t.Fatal(err)
	}
	if got := SessionHarvest(strings.NewReader(harvestPayload(tr, cwd)), &bytes.Buffer{}); got != ExitProceed {
		t.Error("a bank write after the prompt must release the gate")
	}
}

func TestSessionStartAutoInitAndNotice(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv(memory.HarvestChildEnv, "")
	repo := filepath.Join(t.TempDir(), "repo")
	if err := exec.Command("git", "init", "-q", repo).Run(); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(repo, "sub")
	_ = os.MkdirAll(sub, 0o755)
	start := func(session, cwd string) string {
		var out bytes.Buffer
		SessionStart(strings.NewReader(fmt.Sprintf(`{"session_id":%q,"cwd":%q}`, session, cwd)), &out)
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
	// Real session ids are UUID-shaped; memoryContext only advances the chain/ResetSeen for those
	// (probe/test payloads like "probe-x" must not clobber it) — see memory.LooksLikeSessionID.
	const (
		s0 = "00000000-0000-4000-8000-000000000000"
		s1 = "11111111-1111-4111-8111-111111111111"
		s2 = "22222222-2222-4222-8222-222222222222"
	)
	if got := start(s0, sub); got != "" || memory.HasBank(sub) {
		t.Errorf("a repo subdir must not be scaffolded: %q", got)
	}
	got := start(s1, repo)
	if !strings.Contains(got, "Memory bank scaffolded at") || !strings.Contains(got, "<!-- activeContext.md -->") {
		t.Fatalf("auto-init surface: %q", got)
	}
	_ = memory.WriteHarvestResult(s1, memory.HarvestResult{Status: memory.HarvestFailed, Time: time.Now(), Log: "/x.log"})
	got = start(s2, repo)
	if strings.Contains(got, "scaffolded") || !strings.Contains(got, "Previous session id: "+s1) || !strings.Contains(got, "did not complete (failed) (log: /x.log)") {
		t.Errorf("notice: %q", got[:min(len(got), 500)])
	}
	t.Setenv(memory.HarvestChildEnv, "1")
	start("child", repo) // a non-UUID harvest-child id: excluded by IsHarvestChild regardless of shape
	if e, ok := readChain(filepath.Join(memory.StateDir(), "last-session")); !ok || e.sessionID != s2 {
		t.Errorf("a harvest child advanced the session chain: %+v ok=%v", e, ok)
	}
}

// TestSessionStartProbeIDDoesNotClobberLastSession: a non-UUID session id (probe/test payloads,
// e.g. "probe-x") must not overwrite last-session or seed a seen-set, while a real UUID still does.
func TestSessionStartProbeIDDoesNotClobberLastSession(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(memory.HarvestChildEnv, "")
	repo := filepath.Join(t.TempDir(), "repo")
	if err := exec.Command("git", "init", "-q", repo).Run(); err != nil {
		t.Fatal(err)
	}
	const real = "33333333-3333-4333-8333-333333333333"
	memoryContext(repo, real, "", time.Now())
	chain := filepath.Join(memory.StateDir(), "last-session")
	if e, ok := readChain(chain); !ok || e.sessionID != real {
		t.Fatalf("a UUID session id must advance last-session: %+v ok=%v", e, ok)
	}
	memoryContext(repo, "probe-x", "", time.Now())
	if e, ok := readChain(chain); !ok || e.sessionID != real {
		t.Errorf("a probe id must not clobber last-session: got %+v, want %s", e, real)
	}
}

// assertPausedSuppressesHarvest drives Stop then SessionEnd for session "s1" against a paused guard and
// asserts neither spawns a harvest or writes a stamp — shared by every "guard pauses the write paths" test.
func assertPausedSuppressesHarvest(t *testing.T, cwd string) {
	t.Helper()
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	writeEdits(t, tr, 20)
	calls := stubSpawn(t, false)
	var errb bytes.Buffer
	if code := SessionHarvest(strings.NewReader(harvestPayload(tr, cwd)), &errb); code != ExitProceed || calls.Load() != 0 || errb.Len() != 0 {
		t.Errorf("paused Stop: code=%d spawns=%d stderr=%q", code, calls.Load(), errb.String())
	}
	if SessionHarvestEnd(strings.NewReader(harvestPayload(tr, cwd))); calls.Load() != 0 {
		t.Error("paused SessionEnd must not spawn")
	}
	if _, ok := readStamp(stampPath("s1")); ok {
		t.Error("paused harvest wrote a stamp")
	}
}

// A raised maintenance marker pauses every memory write path: no scaffold, no spawn, no block, no stamp —
// the notice alone is injected, and an existing surface still loads read-only behind it.
func TestMemoryGuardPausesScaffoldAndHarvest(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(memory.HarvestChildEnv, "")
	repo := filepath.Join(t.TempDir(), "repo")
	if err := exec.Command("git", "init", "-q", repo).Run(); err != nil {
		t.Fatal(err)
	}
	if err := memory.SetMaintenance("test pause"); err != nil {
		t.Fatal(err)
	}
	got := memoryContext(repo, "s1", "", time.Now())
	if memory.HasBank(repo) || !strings.Contains(got, "memory maintenance in progress (test pause)") || strings.Contains(got, "scaffolded") {
		t.Fatalf("paused SessionStart must emit the notice alone and scaffold nothing: %q", got)
	}
	assertPausedSuppressesHarvest(t, repo)
	if err := memory.ClearMaintenance(); err != nil {
		t.Fatal(err)
	}
	if _, err := memory.Init(repo); err != nil {
		t.Fatal(err)
	}
	if err := memory.SetMaintenance("again"); err != nil {
		t.Fatal(err)
	}
	got = memoryContext(repo, "s2", "", time.Now())
	if !strings.HasPrefix(got, "memory maintenance in progress (again)") || !strings.Contains(got, "<!-- activeContext.md -->") {
		t.Errorf("paused with a bank: notice must prefix the read-only surface: %q", got[:min(len(got), 300)])
	}
}

// An unfinished migration journal pauses the same paths as the maintenance marker (plan §4: "no implicit
// migration") — SessionStart never resumes/completes a migration itself, it only surfaces the notice.
func TestMemoryGuardPausesOnIncompleteMigrationJournal(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(memory.HarvestChildEnv, "")
	repo := filepath.Join(t.TempDir(), "repo")
	if err := exec.Command("git", "init", "-q", repo).Run(); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(memory.MigrateStateDir(), "20260101-000000.journal.json")
	if err := os.MkdirAll(filepath.Dir(journal), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(journal, []byte(`{"phase":"copy","manifest":"/m/pending.json"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	got := memoryContext(repo, "s1", "", time.Now())
	if memory.HasBank(repo) || !strings.Contains(got, "memory migration incomplete") || !strings.Contains(got, "memory migrate --manifest /m/pending.json") ||
		strings.Contains(got, "scaffolded") {
		t.Fatalf("paused SessionStart must emit the migration notice alone and scaffold nothing: %q", got)
	}

	assertPausedSuppressesHarvest(t, repo)

	// A complete journal retires the legacy bank; until the cutover re-points Dir/HomeBankDir at the store, the
	// hook stays paused so SessionStart never recreates it.
	if err := os.WriteFile(journal, []byte(`{"phase":"complete","manifest":"/m/pending.json"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got = memoryContext(repo, "s2", "", time.Now())
	if memory.HasBank(repo) || !strings.Contains(got, "memory migration complete") || strings.Contains(got, "scaffolded") {
		t.Fatalf("a completed journal must keep the pause and scaffold nothing: %q", got[:min(len(got), 300)])
	}
	if _, err := os.Stat(memory.HomeBankDir()); !os.IsNotExist(err) {
		t.Error("SessionStart recreated the retired legacy bank")
	}
	assertPausedSuppressesHarvest(t, repo)
}

// cutoverStore marks the migration complete and creates the unified store's git repo — the state
// memoryContext checks via memory.StartupGuard()/CutOver() before repairing a native link.
func cutoverStore(t *testing.T) {
	t.Helper()
	journal := filepath.Join(memory.MigrateStateDir(), "20260101-000000.journal.json")
	if err := os.MkdirAll(filepath.Dir(journal), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(journal, []byte(`{"phase":"complete","manifest":"/m/done.json"}`), 0o600); err != nil {
		t.Fatal(err)
	}
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
	t.Setenv(memory.HarvestChildEnv, "")
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

	if g := memory.StartupGuard(); g.Paused || !g.Migrated {
		t.Fatalf("test setup: guard = %+v, want unpaused and migrated", g)
	}

	memoryContext(repo, "s1", "", time.Now())

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
	t.Setenv(memory.HarvestChildEnv, "")
	cutoverStore(t)
	repo := filepath.Join(t.TempDir(), "proj")
	if err := exec.Command("git", "init", "-q", repo).Run(); err != nil {
		t.Fatal(err)
	}

	memoryContext(repo, "first", "", time.Now())

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

// TestPreCompactHarvestsOnlyUncoveredEdits: a compaction dispatches the harvest when edits exist past the last
// stamp, never re-dispatches covered ones, never blocks, and stays quiet while memory is paused.
func TestPreCompactHarvestsOnlyUncoveredEdits(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(memory.HarvestChildEnv, "")
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	cwd := t.TempDir()
	calls := stubSpawn(t, true)
	run := func() int { return PreCompact(strings.NewReader(harvestPayload(tr, cwd))) }

	writeEdits(t, tr, 0)
	if run() != ExitProceed || calls.Load() != 0 {
		t.Fatal("no edits must not spawn")
	}
	writeEdits(t, tr, 1) // below the Stop floor: a compaction still checkpoints it
	if run() != ExitProceed || calls.Load() != 1 {
		t.Fatalf("uncovered edit must spawn once, calls=%d", calls.Load())
	}
	if run(); calls.Load() != 1 {
		t.Error("covered edits re-dispatched")
	}
	writeEdits(t, tr, 4)
	if run(); calls.Load() != 2 {
		t.Errorf("new edits after the stamp must spawn again, calls=%d", calls.Load())
	}
	if err := memory.SetMaintenance("test"); err != nil {
		t.Fatal(err)
	}
	writeEdits(t, tr, 9)
	if run() != ExitProceed || calls.Load() != 2 {
		t.Error("paused memory must not spawn")
	}
}

// idle backdates tr's mtime so catchUpPrevious treats the transcript as quiet.
func idle(t *testing.T, path string, d time.Duration) {
	t.Helper()
	at := time.Now().Add(-d)
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

// TestCatchUpPreviousTable drives catchUpPrevious directly against the spawnHarvest seam: each case is a
// distinct reason the previous session's harvest either is or isn't missed/failed.
func TestCatchUpPreviousTable(t *testing.T) {
	const prevID = "prev-session"
	const curID = "cur-session"

	newTranscript := func(t *testing.T, edits int, idleFor time.Duration) string {
		t.Helper()
		tr := filepath.Join(t.TempDir(), "t.jsonl")
		writeEdits(t, tr, edits)
		idle(t, tr, idleFor)
		return tr
	}

	cases := []struct {
		name      string
		setup     func(t *testing.T, tr string)
		edits     int
		idleFor   time.Duration
		curID     string
		noTr      bool // prev.transcriptPath == ""
		wantSpawn bool
		wantNote  bool
	}{
		{
			name:      "missed harvest spawns",
			edits:     5,
			idleFor:   20 * time.Minute,
			wantSpawn: true,
			wantNote:  true,
		},
		{
			name: "already covered by stamp: no spawn",
			setup: func(t *testing.T, tr string) {
				writeStamp(stampPath(prevID), time.Now().Add(-20*time.Minute), "bg", 5)
			},
			edits:   5,
			idleFor: 20 * time.Minute,
		},
		{
			name:    "fresh transcript (<15min): no spawn",
			edits:   5,
			idleFor: 2 * time.Minute,
		},
		{
			name:    "same session id: no spawn",
			edits:   5,
			idleFor: 20 * time.Minute,
			curID:   prevID,
		},
		{
			name:    "fewer than 3 edits: no spawn",
			edits:   2,
			idleFor: 20 * time.Minute,
		},
		{
			name: "failed result retries once",
			setup: func(t *testing.T, tr string) {
				_ = memory.WriteHarvestResult(prevID, memory.HarvestResult{Status: memory.HarvestFailed, Time: time.Now()})
			},
			edits:     2, // below the floor on its own: the failed result is what drives the retry
			idleFor:   20 * time.Minute,
			wantSpawn: true,
			wantNote:  true,
		},
		{
			name: "failed result already retried (catchup stamp): no second spawn",
			setup: func(t *testing.T, tr string) {
				_ = memory.WriteHarvestResult(prevID, memory.HarvestResult{Status: memory.HarvestFailed, Time: time.Now()})
				writeStamp(stampPath(prevID), time.Now().Add(-20*time.Minute), "catchup", 2)
			},
			edits:   2,
			idleFor: 20 * time.Minute,
		},
		{
			name:    "legacy/empty transcript path: no spawn, no crash",
			edits:   5,
			idleFor: 20 * time.Minute,
			noTr:    true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			t.Setenv(memory.HarvestChildEnv, "")
			tr := newTranscript(t, tc.edits, tc.idleFor)
			if tc.setup != nil {
				tc.setup(t, tr)
			}
			calls := stubSpawn(t, true)
			prev := chainEntry{sessionID: prevID, transcriptPath: tr, cwd: t.TempDir()}
			if tc.noTr {
				prev.transcriptPath = ""
			}
			cur := curID
			if tc.curID != "" {
				cur = tc.curID
			}
			note := catchUpPrevious(prev, cur, time.Now())
			gotSpawn := calls.Load() == 1
			if gotSpawn != tc.wantSpawn {
				t.Errorf("spawn = %v, want %v (calls=%d)", gotSpawn, tc.wantSpawn, calls.Load())
			}
			if (note != "") != tc.wantNote {
				t.Errorf("note = %q, want non-empty=%v", note, tc.wantNote)
			}
		})
	}
}

// TestCatchUpPreviousSpawnFailureNoStamp: a failed spawn must not leave a "catchup" stamp behind, so the
// next SessionStart still sees it as missed and retries rather than silently giving up forever.
func TestCatchUpPreviousSpawnFailureNoStamp(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(memory.HarvestChildEnv, "")
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	writeEdits(t, tr, 5)
	idle(t, tr, 20*time.Minute)
	stubSpawn(t, false) // spawn fails
	prev := chainEntry{sessionID: "prev2", transcriptPath: tr, cwd: t.TempDir()}
	if note := catchUpPrevious(prev, "cur2", time.Now()); note != "" {
		t.Errorf("a failed spawn must not report success: %q", note)
	}
	if _, ok := readStamp(stampPath("prev2")); ok {
		t.Error("a failed spawn must not write a catchup stamp")
	}
}

// TestSessionStartCatchUpHarvestChildNeverSpawns: memoryContext gates catchUpPrevious behind
// !IsHarvestChild — a harvest worker's own SessionStart must never dispatch another harvest.
func TestSessionStartCatchUpHarvestChildNeverSpawns(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(memory.HarvestChildEnv, "")
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
	writeChain(chain, chainEntry{sessionID: "prev3", transcriptPath: tr, cwd: repo})
	calls := stubSpawn(t, true)

	t.Setenv(memory.HarvestChildEnv, "1")
	got := memoryContext(repo, "harvest-worker", "", time.Now())
	if calls.Load() != 0 {
		t.Errorf("a harvest child must never catch up the previous session, calls=%d", calls.Load())
	}
	if strings.Contains(got, "Catching up") {
		t.Errorf("a harvest child's SessionStart must not surface the catch-up note: %q", got)
	}
}

// TestSessionStartChainRewrittenWithTranscriptAndCWD: memoryContext's writeChain must persist all three
// fields so the NEXT SessionStart can resolve the previous session's transcript for its own catch-up check.
func TestSessionStartChainRewrittenWithTranscriptAndCWD(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(memory.HarvestChildEnv, "")
	cwd := t.TempDir()
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	writeEdits(t, tr, 1)
	const sid = "44444444-4444-4444-8444-444444444444"

	memoryContext(cwd, sid, tr, time.Now())

	chain := filepath.Join(memory.StateDir(), "last-session")
	e, ok := readChain(chain)
	if !ok || e.sessionID != sid || e.transcriptPath != tr || e.cwd != cwd {
		t.Errorf("chain = %+v ok=%v, want {%s %s %s}", e, ok, sid, tr, cwd)
	}
}

// TestReadChainLegacyOneLineFormat: a pre-upgrade chain file (bare session id, no transcript/cwd lines)
// still parses, and catchUpPrevious treats its blank transcript path as "nothing to check" rather than crashing.
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
	if note := catchUpPrevious(e, "new-id", time.Now()); note != "" {
		t.Errorf("a legacy chain entry must not attempt a catch-up: %q", note)
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
	code := SessionHarvest(strings.NewReader(fmt.Sprintf(`{"session_id":"x","cwd":%q,"transcript_path":%q}`, cwd, tr)), &errb)
	if code != ExitBlock && code != ExitProceed {
		t.Errorf("unexpected exit code %d parsing a catchup stamp", code)
	}
}
