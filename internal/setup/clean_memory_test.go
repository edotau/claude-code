package setup

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
)

// sweepSandboxes runs a dry run or a real sweep of the sandbox target and returns its report.
func sweepSandboxes(t *testing.T, dry bool) string {
	t.Helper()
	args := []string{"--target", "memory-sandboxes"}
	if dry {
		args = append(args, "--dry-run")
	}
	var out, errw bytes.Buffer
	if code := Clean(args, &out, &errw); code != 0 {
		t.Fatalf("clean %v = %d:\n%s", args, code, errw.String())
	}
	return errw.String()
}

// TestCleanMemorySandboxKeepsModified: a sandbox whose memory repo gained a commit or an untracked file is kept.
func TestCleanMemorySandboxKeepsModified(t *testing.T) {
	_ = newSandboxFixture(t)
	committed, untracked := makeSandbox(t, 40*24*time.Hour), makeSandbox(t, 40*24*time.Hour)
	hosttest.In(t, filepath.Join(committed, "memory"), "-c", "user.name=x", "-c", "user.email=x@x",
		"commit", "--allow-empty", "-m", "later work")
	write(t, filepath.Join(untracked, "memory", "scratch.md"), "mine")
	old := time.Now().Add(-40 * 24 * time.Hour)
	_ = os.Chtimes(filepath.Join(untracked, "memory", "scratch.md"), old, old)
	if rep := sweepSandboxes(t, true); !strings.Contains(rep, "kept: modified") {
		t.Errorf("dry run does not list the kept sandboxes as modified:\n%s", rep)
	}
	sweepSandboxes(t, false)
	if !exists(t, committed) || !exists(t, untracked) {
		t.Errorf("modified sandbox deleted: committed kept=%v untracked kept=%v", exists(t, committed), exists(t, untracked))
	}
}

// TestCleanMemorySandboxKeepsLiveOwner: a marker naming a live process keeps the sandbox; no marker means age only.
func TestCleanMemorySandboxKeepsLiveOwner(t *testing.T) {
	fx := newSandboxFixture(t)
	stubPidAlive(t, func(int) bool { return true })
	sweepSandboxes(t, false)
	if !exists(t, fx.aged) {
		t.Fatal("sandbox owned by a live process was deleted")
	}
	if err := os.Remove(filepath.Join(fx.aged, memory.SandboxOwnerFile)); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-40 * 24 * time.Hour)
	if err := os.Chtimes(fx.aged, old, old); err != nil { // removing the marker touched the dir
		t.Fatal(err)
	}
	sweepSandboxes(t, false)
	if exists(t, fx.aged) {
		t.Error("an aged sandbox with no marker was kept")
	}
}

// TestCleanMemorySandboxFoundThroughRegistry: a sandbox made under another TMPDIR is swept and its line then dropped.
func TestCleanMemorySandboxFoundThroughRegistry(t *testing.T) {
	_ = newSandboxFixture(t)
	t.Setenv("TMPDIR", t.TempDir())
	far := makeSandbox(t, 40*24*time.Hour)
	gone := filepath.Join(t.TempDir(), sandboxPrefix+"gone")
	if err := paths.AppendLine(memory.SandboxRegistry(), []byte(gone)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", t.TempDir())
	sweepSandboxes(t, false)
	if exists(t, far) {
		t.Error("sandbox under another TMPDIR was not swept")
	}
	if data, err := os.ReadFile(memory.SandboxRegistry()); err != nil || strings.Contains(string(data), "gone") {
		t.Errorf("registry still names a missing dir: %q %v", data, err)
	}
}

// TestCleanMemorySandboxRegistryNeverDeletesNonSandbox: a registry line naming a non-sandbox dir is left alone.
func TestCleanMemorySandboxRegistryNeverDeletesNonSandbox(t *testing.T) {
	_ = newSandboxFixture(t)
	victim := filepath.Join(t.TempDir(), sandboxPrefix+"victim")
	write(t, filepath.Join(victim, "precious.txt"), "x")
	old := time.Now().Add(-40 * 24 * time.Hour)
	_ = os.Chtimes(filepath.Join(victim, "precious.txt"), old, old)
	_ = os.Chtimes(victim, old, old)
	if _, err := memory.UpdateSandboxRegistry(victim); err != nil {
		t.Fatal(err)
	}
	sweepSandboxes(t, false)
	if !exists(t, victim) {
		t.Error("a registry path without the seed commit was deleted")
	}
}
