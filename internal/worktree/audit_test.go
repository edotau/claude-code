package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

func auditFixtureRepo(t *testing.T) (dir, head string) {
	t.Helper()
	dir = t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "x"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Makefile"), []byte("audit:\n\ttrue\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, strings.TrimSpace(string(out))
}

// A spooled audit blocks a second start of the same sha; with no worker it is not reported.
func TestAuditStartsOnce(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Cleanup(SwapAuditCommand("echo ok"))
	t.Cleanup(SwapAuditSpawn(func() bool { return true }))
	main, sha := auditFixtureRepo(t)
	if line := startAudit(main, sha); !strings.HasPrefix(line, "audit: started on "+ShortOID(sha)) {
		t.Fatalf("start = %q", line)
	}
	if line := startAudit(main, sha); !strings.HasPrefix(line, "audit: already running") {
		t.Fatalf("second start = %q", line)
	}
	if items, err := ReadAuditSpool(AuditSpool()); err != nil || len(items) != 1 || items[0].SHA != sha {
		t.Fatalf("spool = %+v, %v", items, err)
	}
	if got := AuditReports(); len(got) != 0 {
		t.Fatalf("reports = %v", got)
	}
}

// A repo whose Makefile has no audit target starts nothing and spools nothing.
func TestAuditSkipsRepoWithoutTarget(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Cleanup(SwapAuditCommand("echo ok"))
	main, sha := auditFixtureRepo(t)
	if err := os.WriteFile(filepath.Join(main, "Makefile"), []byte("build:\n\ttrue\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if line := startAudit(main, sha); line != "audit: skipped, no audit target in "+main {
		t.Fatalf("skip = %q", line)
	}
	if paths.FileExists(AuditSpool()) {
		t.Fatal("skipped audit spooled a line")
	}
}

// A published nonzero row reports FAILED with the log path and its shared run, once.
func TestAuditReportsFailure(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	row := AuditRow{Repo: "/r", SHA: "fedcba9876543210", Queued: 1, State: AuditDone, Ended: time.Now().Unix(),
		Log: "/l.log", With: []string{"0123456789abcdef"}, Code: 2, Secs: 5}
	if err := WriteAuditSnapshot([]AuditRow{row}); err != nil {
		t.Fatal(err)
	}
	if got := AuditReports(); len(got) != 1 || !strings.HasSuffix(got[0], "FAILED (exit 2) in /r: /l.log — fix on main now"+runWith(row.With)) {
		t.Fatalf("failed report = %v", got)
	}
	if again := AuditReports(); len(again) != 0 {
		t.Fatalf("second report = %v", again)
	}
}

// A running row whose log is older than the threshold reports hung once; with no worker alive one spawns, once.
func TestAuditReportsHung(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	spawned := 0
	t.Cleanup(SwapAuditSpawn(func() bool { spawned++; return true }))
	old := auditHungAfter
	auditHungAfter = time.Minute
	t.Cleanup(func() { auditHungAfter = old })
	log := filepath.Join(t.TempDir(), "run.log")
	if err := os.WriteFile(log, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(log, past, past); err != nil {
		t.Fatal(err)
	}
	row := AuditRow{SHA: "0123456789abcdef", Queued: time.Now().UnixNano(), State: AuditRunning, Log: log}
	if err := WriteAuditSnapshot([]AuditRow{row}); err != nil {
		t.Fatal(err)
	}
	if got := AuditReports(); len(got) != 1 || !strings.Contains(got[0], "hung") || !strings.Contains(got[0], log) {
		t.Fatalf("hung report = %v", got)
	}
	if again := AuditReports(); len(again) != 0 || spawned != 1 { // the second prompt is inside the cooldown
		t.Fatalf("second report = %v, spawned %d, want 1", again, spawned)
	}
}
