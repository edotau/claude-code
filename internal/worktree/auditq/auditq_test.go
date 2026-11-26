package auditq

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/worktree"
	"github.com/edotau/claude-code/internal/worktree/worktreetest"
)

func TestMain(m *testing.M) {
	cleanup := hosttest.Setup()
	scan := worktree.ProcessScan
	worktreetest.StubProcessScanForTest(scan.In, scan.Up, scan.Procs, scan.Agents)
	code := m.Run()
	cleanup()
	hosttest.Cleanup()
	os.Exit(code)
}

// auditRepo is a repo with an audit target and two lane commits a and b off its HEAD, which stays at root.
func auditRepo(t *testing.T) (dir, a, b string) {
	t.Helper()
	dir = t.TempDir()
	git(t, dir, "init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "Makefile"), []byte("audit:\n\ttrue\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "Makefile")
	git(t, dir, "commit", "-q", "-m", "root")
	root := git(t, dir, "rev-parse", "HEAD")
	return dir, laneCommit(t, dir, root, "a.txt"), laneCommit(t, dir, root, "b.txt")
}

func laneCommit(t *testing.T, dir, root, name string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", name)
	git(t, dir, "commit", "-q", "-m", name)
	sha := git(t, dir, "rev-parse", "HEAD")
	git(t, dir, "reset", "-q", "--hard", root)
	return sha
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// startBoth spools a and b as two lands would, so the test's own workers drain them.
func startBoth(t *testing.T, cmd string) (dir, a, b string) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Cleanup(worktree.SwapAuditCommand(cmd))
	dir, a, b = auditRepo(t)
	root := git(t, dir, "rev-parse", "HEAD")
	for _, sha := range []string{a, b} {
		line, err := json.Marshal(worktree.AuditItem{Repo: dir, Base: root, SHA: sha, Queued: time.Now().UnixNano()})
		if err == nil {
			err = paths.AppendLine(worktree.AuditSpool(), line)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	return dir, a, b
}

// Two lands drained by two concurrent workers run as one audit at HEAD with both diffs applied; each reports passed.
func TestAuditCoalescesBurst(t *testing.T) {
	_, a, b := startBoth(t, `test -f a.txt && test -f b.txt && test -z "$(git status --porcelain)"`)
	var wg sync.WaitGroup
	wg.Go(Run)
	wg.Go(Run)
	wg.Wait()
	got := strings.Join(worktree.AuditReports(), "\n")
	for _, want := range []string{"audit of " + worktree.ShortOID(a) + " passed (", "s) (run with " + worktree.ShortOID(a) + ")",
		"audit of " + worktree.ShortOID(b) + " passed ("} {
		if !strings.Contains(got, want) {
			t.Fatalf("reports %q lack %q", got, want)
		}
	}
	logs, _ := filepath.Glob(filepath.Join(worktree.AuditDir(), "run-*.log"))
	if len(logs) != 1 || len(worktree.AuditReports()) != 0 {
		t.Fatalf("run logs %v, want one shared run reported once", logs)
	}
}

// The slot sees the primary checkout's untracked .venv through a symlink, and the original survives cleanup.
func TestAuditSlotLinksVenv(t *testing.T) {
	dir, _, _ := startBoth(t, "test -L .venv && test -f .venv/marker")
	marker := filepath.Join(dir, ".venv", "marker")
	if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	Run()
	if got := worktree.AuditReports(); len(got) != 2 || !strings.Contains(got[0], "passed") {
		t.Fatalf("reports = %v", got)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("slot cleanup removed the primary .venv")
	}
}

// A finished file-based record from before the table is imported once, reported, and its files removed.
func TestAuditImportsLegacyRecord(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	base := filepath.Join(worktree.AuditDir(), "abcdef01")
	rec := fmt.Sprintf(`{"sha":"abcdef0123456789","repo":"/x","log":"/x/a.log","started":%d}`, time.Now().Unix())
	if err := os.MkdirAll(worktree.AuditDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	for ext, body := range map[string]string{".json": rec, ".exit": "3 9\n"} {
		if err := os.WriteFile(base+ext, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	Run()
	if got := worktree.AuditReports(); len(got) != 1 || !strings.Contains(got[0], "audit of abcdef01 FAILED (exit 3) in /x: /x/a.log") {
		t.Fatalf("reports = %v", got)
	}
	if _, err := os.Stat(base + ".json"); err == nil {
		t.Fatal("legacy record kept after import")
	}
}

// A failing audit ends every covered sha FAILED with its exit code.
func TestAuditReportsFailedRun(t *testing.T) {
	_, a, b := startBoth(t, "exit 2")
	Run()
	got := worktree.AuditReports()
	if len(got) != 2 || !strings.Contains(got[0], "FAILED (exit 2)") || !strings.Contains(got[1], "FAILED (exit 2)") {
		t.Fatalf("reports for %s %s = %v", a, b, got)
	}
	if got, _ := worktree.ListFailures(worktree.FailuresDir()); len(got) != 2 || got[0].Kind != worktree.FailAudit {
		t.Fatalf("two failed rows must broadcast two audit failures once each: %+v", got)
	}
	Run()
	worktree.AuditReports()
	if got, _ := worktree.ListFailures(worktree.FailuresDir()); len(got) != 2 {
		t.Fatalf("a second pass re-reported: %+v", got)
	}
}

// A dead worker's running row stays running while a process is in its slot, then reruns once the slot is free.
func TestAuditOrphanSlotFence(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Cleanup(worktree.SwapAuditCommand("true"))
	dir, a, _ := auditRepo(t)
	slot := t.TempDir()
	db, err := open(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`INSERT INTO audits(sha, queued, repo, base, state, slot) VALUES(?, 1, ?, ?, 'running', ?)`,
		a, dir, git(t, dir, "rev-parse", "HEAD"), slot); err != nil {
		t.Fatal(err)
	}
	busy := true
	in := worktree.ProcessScan.In
	old := *in
	t.Cleanup(func() { *in = old })
	*in = func(d string) ([]worktree.Proc, error) {
		if busy && d == paths.CanonDeep(slot) {
			return []worktree.Proc{{PID: 1 << 30, Comm: "orphan"}}, nil
		}
		return nil, nil
	}
	state := func() (s string, code int) {
		if err := db.QueryRow(`SELECT state, code FROM audits WHERE sha = ?`, a).Scan(&s, &code); err != nil {
			t.Fatal(err)
		}
		return s, code
	}
	Run()
	if s, _ := state(); s != worktree.AuditRunning {
		t.Fatalf("busy slot: state %q, want running", s)
	}
	busy = false
	Run()
	if s, code := state(); s != worktree.AuditDone || code != 0 {
		t.Fatalf("free slot: state %q code %d, want done 0", s, code)
	}
}

// With every slot and the fallback tree busy, spooled rows stay queued, never failed, and run once a slot frees.
func TestAuditBusySlotsRequeue(t *testing.T) {
	dir, a, b := startBoth(t, "true")
	t.Cleanup(worktree.SwapAuditSpawn(func() bool { return true })) // the report's restart is not this test's
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(dir)))[:8]
	for i := range 4 { // existing slots, so Reset checks each one for a live process
		slot := filepath.Join(worktree.SlotRoot(), fmt.Sprintf("slot-%s-%d", key, i))
		if err := os.MkdirAll(slot, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(slot, ".git"), []byte("gitdir: /nowhere\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	scan := worktree.ProcessScan
	oldIn, oldSettle := *scan.In, *scan.Settle
	t.Cleanup(func() { *scan.In, *scan.Settle = oldIn, oldSettle })
	root := paths.CanonDeep(worktree.SlotRoot())
	*scan.Settle = 0
	*scan.In = func(d string) ([]worktree.Proc, error) {
		if strings.HasPrefix(d, root) {
			return []worktree.Proc{{PID: 1 << 30, Comm: "orphan"}}, nil
		}
		return nil, nil
	}
	Run()
	if got := worktree.AuditReports(); len(got) != 0 {
		t.Fatalf("busy slots reported %v, want nothing", got)
	}
	if got, _ := worktree.ListFailures(worktree.FailuresDir()); len(got) != 0 {
		t.Fatalf("a busy slot is not a failure: %+v", got)
	}
	*scan.In = oldIn
	Run()
	got := strings.Join(worktree.AuditReports(), "\n")
	for _, sha := range []string{a, b} {
		if !strings.Contains(got, "audit of "+worktree.ShortOID(sha)+" passed") {
			t.Fatalf("after the slot freed, reports %q lack %s passed", got, worktree.ShortOID(sha))
		}
	}
}
