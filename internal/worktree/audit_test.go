package worktree

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// A started audit blocks a second start, then reports once when its exit file lands.
func TestAuditStartsOnceAndReportsOnce(t *testing.T) {
	old := auditCommand
	auditCommand = "echo ok"
	t.Cleanup(func() { auditCommand = old })
	sha, main := "0123456789abcdef", t.TempDir()
	if line := StartAudit(main, sha); !strings.HasPrefix(line, "audit: started on 01234567") {
		t.Fatalf("start = %q", line)
	}
	if line := StartAudit(main, sha); !strings.HasPrefix(line, "audit: already running") {
		t.Fatalf("second start = %q", line)
	}
	for deadline := time.Now().Add(10 * time.Second); !paths.FileExists(auditBase(sha)+".exit") && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
	if got := AuditReports(); len(got) != 1 || !strings.Contains(got[0], "audit of 01234567 passed") {
		t.Fatalf("reports = %v", got)
	}
	if again := AuditReports(); len(again) != 0 {
		t.Fatalf("second report = %v", again)
	}
}

// A nonzero exit file reports FAILED with the log path.
func TestAuditReportsFailure(t *testing.T) {
	base := auditBase("fedcba9876543210")
	if err := os.MkdirAll(auditDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{".json": `{"sha":"fedcba9876543210","log":"/l.log"}`, ".exit": "2 5\n"} {
		if err := os.WriteFile(base+name, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got := AuditReports(); len(got) != 1 || !strings.Contains(got[0], "FAILED (exit 2): /l.log") {
		t.Fatalf("failed report = %v (dir %s)", got, filepath.Dir(base))
	}
}
