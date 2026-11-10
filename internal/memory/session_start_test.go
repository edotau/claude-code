package memory

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sessionStartContext runs StartSession and returns the context it renders ("" when silent).
func sessionStartContext(t *testing.T, session, cwd string) string {
	t.Helper()
	return StartSession(session, cwd, time.Now())
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
	t.Setenv(UpdateChildEnv, "")
	cutoverStore(t)
	sub := filepath.Join(repo, "sub")
	_ = os.MkdirAll(sub, 0o755)
	// Only UUID-shaped ids advance the chain and ResetSeen; probe ids like "probe-x" never clobber it.
	const (
		s0 = "00000000-0000-4000-8000-000000000000"
		s1 = "11111111-1111-4111-8111-111111111111"
		s2 = "22222222-2222-4222-8222-222222222222"
	)
	if got := sessionStartContext(t, s0, sub); got != "" || HasBank(sub) {
		t.Errorf("a repo subdir must not be scaffolded: %q", got)
	}
	// the unified surface is catalog + focus only; seed a catalog so the scaffold notice has a surface to ride on
	if err := os.WriteFile(filepath.Join(StoreDir(), CatalogFile), []byte("# Memory catalog\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := sessionStartContext(t, s1, repo)
	if !strings.Contains(got, "Memory bank scaffolded at") {
		t.Fatalf("auto-init surface: %q", got)
	}
	_ = WriteUpdateResult(s1, UpdateResult{Status: UpdateFailed, Time: time.Now(), Log: "/x.log"})
	got = sessionStartContext(t, s2, repo)
	if strings.Contains(got, "scaffolded") {
		t.Errorf("notice must not re-announce the scaffold: %q", got[:min(len(got), 500)])
	}
	for _, want := range []string{"Previous session id: " + s1, "did not complete (failed) (log: /x.log)"} {
		if !strings.Contains(got, want) {
			t.Errorf("notice lacks %q: %q", want, got[:min(len(got), 500)])
		}
	}
	t.Setenv(UpdateChildEnv, "1")
	sessionStartContext(t, "child", repo) // a non-UUID update-child id: excluded by IsUpdateChild regardless of shape
	if e, ok := readChain(filepath.Join(StateDir(), "last-session")); !ok || e != s2 {
		t.Errorf("an update child advanced the session chain: %+v ok=%v", e, ok)
	}
}

// A non-UUID (probe) id never overwrites last-session or seeds a seen-set; a real UUID still does.
func TestSessionStartProbeIDDoesNotClobberLastSession(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(UpdateChildEnv, "")
	repo := filepath.Join(t.TempDir(), "repo")
	if err := exec.Command("git", "init", "-q", repo).Run(); err != nil {
		t.Fatal(err)
	}
	const real = "33333333-3333-4333-8333-333333333333"
	StartSession(real, repo, time.Now())
	chain := filepath.Join(StateDir(), "last-session")
	if e, ok := readChain(chain); !ok || e != real {
		t.Fatalf("a UUID session id must advance last-session: %+v ok=%v", e, ok)
	}
	StartSession("probe-x", repo, time.Now())
	if e, ok := readChain(chain); !ok || e != real {
		t.Errorf("a probe id must not clobber last-session: got %+v, want %s", e, real)
	}
}

// cutoverStore creates the unified store's git repo, the state memoryContext needs before repairing a native link.
func cutoverStore(t *testing.T) {
	t.Helper()
	if err := StoreRepo().Ensure(); err != nil {
		t.Fatal(err)
	}
}

// After the cutover StartSession repairs the cwd's native memory link to its repo store dir.
func TestSessionStartLinksNativeMemoryAfterCutover(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv(UpdateChildEnv, "")
	cutoverStore(t)

	repo := filepath.Join(t.TempDir(), "repo")
	if err := exec.Command("git", "init", "-q", repo).Run(); err != nil {
		t.Fatal(err)
	}
	// Pre-scaffold the store (steady state: a .origin exists) so this test isolates the link-repair contract.
	if _, err := EnsureRepoDir(repo); err != nil {
		t.Fatal(err)
	}

	StartSession("s1", repo, time.Now())

	res, ok := LinkNativeFor(repo)
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

// A repo's first start after the cutover links native memory to Init's scaffolded store, never a <slug>--<hash> dir.
func TestSessionStartFirstSessionLinksScaffoldedStore(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv(UpdateChildEnv, "")
	cutoverStore(t)
	repo := filepath.Join(t.TempDir(), "proj")
	if err := exec.Command("git", "init", "-q", repo).Run(); err != nil {
		t.Fatal(err)
	}

	StartSession("first", repo, time.Now())

	dir := Dir(repo)
	if _, err := ReadOrigin(dir); err != nil {
		t.Fatalf("store %s has no .origin after the first SessionStart: %v", dir, err)
	}
	if strings.Contains(filepath.Base(dir), "--") {
		t.Errorf("store key got a collision suffix on a fresh repo: %s", dir)
	}
	res, ok := LinkNativeFor(repo)
	if !ok || res.Action != "ok" || res.Target != dir {
		t.Errorf("native link after first SessionStart = %+v ok=%v, want ok -> %s", res, ok, dir)
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

// An update worker's own SessionStart never dispatches the catch-up sweep.
func TestSessionStartCatchUpUpdateChildNeverSpawns(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(UpdateChildEnv, "")
	repo := filepath.Join(t.TempDir(), "repo")
	if err := exec.Command("git", "init", "-q", repo).Run(); err != nil {
		t.Fatal(err)
	}
	if _, err := Init(repo); err != nil {
		t.Fatal(err)
	}
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	writeEdits(t, tr, 5)
	idle(t, tr, 20*time.Minute)
	chain := filepath.Join(StateDir(), "last-session")
	writeChain(chain, "prev3")
	var sweeps int
	orig := spawn.catchup
	spawn.catchup = func(string, string) bool { sweeps++; return true }
	t.Cleanup(func() { spawn.catchup = orig })

	t.Setenv(UpdateChildEnv, "1")
	got := StartSession("44444444-4444-4444-8444-444444444445", repo, time.Now())
	if sweeps != 0 {
		t.Errorf("an update child must never spawn the catch-up sweep, calls=%d", sweeps)
	}
	if strings.Contains(got, "Catching up") {
		t.Errorf("an update child's SessionStart must not surface the catch-up note: %q", got)
	}
}

// The session surface carries no all-repos catalog rows, however many repos the catalog lists.
func TestSessionStartCarriesNoCatalogRows(t *testing.T) {
	recallEnv(t)
	var cat strings.Builder
	cat.WriteString("# Memory catalog\n\n## Repos\n\n")
	for i := range 120 {
		fmt.Fprintf(&cat, "- repo-%03d — some description of the repo · last 2026-10-29 → repo-%03d/MEMORY.md\n", i, i)
	}
	if err := os.MkdirAll(StoreDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(StoreDir(), CatalogFile), []byte(cat.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	got := StartSession("probe-x", t.TempDir(), time.Now())
	if strings.Contains(got, "repo-000") || strings.Contains(got, "repo-119") {
		t.Errorf("catalog rows injected:\n%s", got)
	}
	if !strings.Contains(got, "other repos:") || len(got) > 1500 {
		t.Errorf("want the one-line pointer and a small surface, got %d bytes:\n%s", len(got), got)
	}
}

// TestStartSessionProbesBankOnce: a banked repo's start probes the bank once for the scaffold and dream checks.
func TestStartSessionProbesBankOnce(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "banked")
	if _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	probes := 0
	orig := hasBank
	hasBank = func(r string) bool { probes++; return orig(r) }
	t.Cleanup(func() { hasBank = orig })
	StartSession("00000000-0000-4000-8000-0000000000a0", root, time.Now())
	if probes != 1 {
		t.Fatalf("bank probes per StartSession = %d, want 1", probes)
	}
}
