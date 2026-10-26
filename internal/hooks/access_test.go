package hooks

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/memory"
)

func readAccessLog(t *testing.T, dir string) string {
	t.Helper()
	data, _ := os.ReadFile(memory.AccessLogPath(dir))
	return string(data)
}

func TestLogReadAccessGlobalBankFile(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	bank := memory.HomeBankDir()
	if err := os.MkdirAll(bank, 0o755); err != nil {
		t.Fatal(err)
	}
	logReadAccess(filepath.Join(bank, "decisionLog.md"), "", time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC))
	if got := readAccessLog(t, bank); !strings.Contains(got, "2026-07-20T00:00:00Z -- bank:decisionLog.md -- read") {
		t.Errorf("bank read not traced: %q", got)
	}
}

func TestLogReadAccessLocalBankFile(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	repo := filepath.Join(t.TempDir(), "repo")
	if err := exec.Command("git", "init", "-q", repo).Run(); err != nil {
		t.Skip("git unavailable:", err)
	}
	local, err := memory.EnsureRepoDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	if local == memory.HomeBankDir() {
		t.Fatal("a repo root must own a local bank")
	}
	logReadAccess(filepath.Join(local, memory.BankIndexFile), repo, time.Now())
	if got := readAccessLog(t, local); !strings.Contains(got, "bank:"+memory.BankIndexFile+" -- read") {
		t.Errorf("local bank read not traced: %q", got)
	}
	if got := readAccessLog(t, memory.HomeBankDir()); got != "" {
		t.Errorf("a local read must not land in the global log: %q", got)
	}
}

func TestLogReadAccessIgnoresOrdinaryFiles(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", root)
	logReadAccess(filepath.Join(root, "other", "decisionLog.md"), "", time.Now())  // bank name outside a bank
	logReadAccess(filepath.Join(memory.HomeBankDir(), "notes.md"), "", time.Now()) // in the bank, not a typed file
	logReadAccess(filepath.Join(root, "internal", "memory", "store.go"), "", time.Now())
	logReadAccess("", "", time.Now())
	if got := readAccessLog(t, memory.HomeBankDir()); got != "" {
		t.Errorf("ordinary reads traced: %q", got)
	}
}

// The hook itself: a Read payload lands one line, a non-Read or malformed payload proceeds silently.
func TestAccessTraceHook(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	bank := memory.HomeBankDir()
	if err := os.MkdirAll(bank, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := fmt.Sprintf(`{"tool_name":"Read","tool_input":{"file_path":%q}}`, filepath.Join(bank, "progress.md"))
	if got := AccessTrace(strings.NewReader(payload)); got != ExitProceed {
		t.Fatalf("exit %d", got)
	}
	if got := AccessTrace(strings.NewReader("not json")); got != ExitProceed {
		t.Error("malformed input must fail open")
	}
	if got := readAccessLog(t, bank); strings.Count(got, "bank:progress.md -- read") != 1 {
		t.Errorf("log %q", got)
	}
}
