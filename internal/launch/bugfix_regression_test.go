package launch

import (
	"bytes"
	"errors"
	"io"
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

func TestInstallBinaryRepairsIdenticalNonExecutableCopy(t *testing.T) {
	hermetic(t, "")
	installTestBinary(t)
	target, err := installBinaryTarget(io.Discard, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := installBinaryTarget(io.Discard, false); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatalf("identical installed copy remained non-executable: %v", info.Mode())
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "binary" {
		t.Fatalf("repaired binary = %q, %v; want original bytes", data, err)
	}
}

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

func TestSeedTmuxConfSkipsNonENOENTProbeFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	xdg := filepath.Join(home, "xdg-loop")
	t.Setenv("XDG_CONFIG_HOME", xdg)
	probeSymlinkLoop(t, xdg)
	var out bytes.Buffer
	seedTmuxConf(&out, false)
	if _, err := os.Lstat(filepath.Join(home, ".tmux.conf")); !os.IsNotExist(err) {
		t.Fatalf("tmux seeded config despite failed XDG probe: Lstat = %v", err)
	}
	if !strings.Contains(out.String(), "skip") || !strings.Contains(out.String(), xdg) {
		t.Fatalf("tmux output = %q, want skip naming failed probe", out.String())
	}
}

func TestStrayClaudeJSONRejectsNonENOENTStatFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	probeSymlinkLoop(t, filepath.Join(home, ".claude"))
	stub := filepath.Join(home, ".claude", ".claude.json")
	ok, detail := strayClaudeJSON()
	if ok || !strings.Contains(detail, "cannot stat "+stub) {
		t.Fatalf("stray config probe = %v, %q; want unhealthy cannot-stat diagnostic", ok, detail)
	}
}
