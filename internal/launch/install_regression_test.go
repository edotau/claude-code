package launch

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/tools"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/router"
)

// A loop produces a deterministic non-ENOENT probe failure even when tests run as root.
func probeSymlinkLoop(t *testing.T, path string) {
	t.Helper()
	if err := os.Symlink(path, path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, syscall.ELOOP) {
		t.Fatalf("fixture stat = %v, want ELOOP", err)
	}
}

func TestToolDoctorRejectsLstatFailure(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	probeSymlinkLoop(t, paths.BinDir())
	entry := filepath.Join(paths.BinDir(), "opencode")
	if _, err := os.Lstat(entry); !errors.Is(err, syscall.ELOOP) {
		t.Fatalf("fixture Lstat = %v, want ELOOP", err)
	}
	if toolDoctorOK(tools.Status{Name: "opencode"}) {
		t.Fatal("tool doctor reported healthy despite non-ENOENT Lstat failure")
	}
}

func TestToolHealthPreservesExhaustedQuotaFailure(t *testing.T) {
	hermetic(t, "")
	t.Setenv("PATH", t.TempDir())
	now := time.Now()
	if err := router.Record("github·regression", 100, "test", now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	var rows []Check
	checkToolHealth(func(ok, hard bool, name, detail string) {
		rows = append(rows, Check{Name: name, OK: ok, Hard: hard, Detail: detail})
	})
	row := findCheck(rows, "quota github·regression")
	if row == nil {
		t.Fatal("missing exhausted quota row")
	}
	if row.OK || row.Hard || !strings.Contains(row.Detail, "exhausted at 100%") {
		t.Fatalf("exhausted quota row = %+v, want failed soft check with exhaustion detail", row)
	}
}
