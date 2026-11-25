package worktree

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A slot is reused at the same path, reset clean, and refused outside the slot root.
func TestSlotAcquireReuseReset(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	top, sha := auditFixtureRepo(t)
	ctx := context.Background()
	a, err := AcquireSlot(ctx, top, sha, "own")
	if err != nil {
		t.Fatal(err)
	}
	dir := a.Dir
	if filepath.Dir(dir) != SlotRoot() {
		t.Fatalf("slot %s not under %s", dir, SlotRoot())
	}
	junk := filepath.Join(dir, "junk")
	if err := os.WriteFile(junk, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	busy, err := AcquireSlot(ctx, top, sha, "own")
	if err != nil {
		t.Fatal(err)
	}
	if busy.Dir == dir {
		t.Fatalf("held slot handed out twice: %s", dir)
	}
	busy.Release()
	a.Release()
	b, err := AcquireSlot(ctx, top, sha, "own")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Release()
	if b.Dir != dir {
		t.Fatalf("slot %s, want reuse of %s", b.Dir, dir)
	}
	if _, err := os.Stat(junk); err == nil {
		t.Fatal("untracked file survived reset")
	}
	if err := (&Slot{Dir: top, top: top}).Reset(ctx, sha); err == nil {
		t.Fatal("reset of a directory outside the slot root was allowed")
	}
}

func slotKey(top string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(top)))[:8] }

// canaryDir is a directory outside the slot root holding a file that must survive any refused acquire.
func canaryDir(t *testing.T) (dir, canary string) {
	t.Helper()
	dir = t.TempDir()
	canary = filepath.Join(dir, "canary")
	if err := os.WriteFile(canary, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, canary
}

func assertRefused(t *testing.T, top, sha, canary string) {
	t.Helper()
	s, err := AcquireSlot(context.Background(), top, sha, "own")
	if err == nil {
		s.Release()
		t.Fatal("acquire succeeded on a hostile slot root")
	}
	if b, rerr := os.ReadFile(canary); rerr != nil || string(b) != "keep" {
		t.Fatalf("canary gone or changed: %q %v", b, rerr)
	}
}

func TestSlotRefusesSymlinkedSlotDir(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	top, sha := auditFixtureRepo(t)
	target, canary := canaryDir(t)
	if err := os.MkdirAll(SlotRoot(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(SlotRoot(), "slot-"+slotKey(top)+"-0")); err != nil {
		t.Skip(err)
	}
	assertRefused(t, top, sha, canary)
}

func TestSlotRefusesSymlinkedLock(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	top, sha := auditFixtureRepo(t)
	_, canary := canaryDir(t)
	if err := os.MkdirAll(SlotRoot(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(canary, filepath.Join(SlotRoot(), "slot-"+slotKey(top)+"-0.lock")); err != nil {
		t.Skip(err)
	}
	assertRefused(t, top, sha, canary)
}

func TestSlotRefusesSymlinkedRoot(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	top, sha := auditFixtureRepo(t)
	target, canary := canaryDir(t)
	if err := os.Symlink(target, SlotRoot()); err != nil {
		t.Skip(err)
	}
	assertRefused(t, top, sha, canary)
}

func TestSlotRefusesWritableRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no mode bits")
	}
	t.Setenv("TMPDIR", t.TempDir())
	top, sha := auditFixtureRepo(t)
	_, canary := canaryDir(t)
	if err := os.MkdirAll(SlotRoot(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(SlotRoot(), 0o777); err != nil {
		t.Fatal(err)
	}
	assertRefused(t, top, sha, canary)
}

func TestSlotResetResolvesSymlinksAndKeepsDotDotNames(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	top, sha := auditFixtureRepo(t)
	ctx := context.Background()
	target, canary := canaryDir(t)
	if err := os.MkdirAll(SlotRoot(), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(SlotRoot(), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skip(err)
	}
	for _, dir := range []string{SlotRoot(), link, SlotRoot() + "-evil", filepath.Join(SlotRoot(), "..")} {
		if err := (&Slot{Dir: dir, top: top}).Reset(ctx, sha); err == nil {
			t.Fatalf("reset of %s was allowed", dir)
		}
	}
	if _, err := os.Stat(canary); err != nil {
		t.Fatalf("canary gone: %v", err)
	}
	dotted := filepath.Join(SlotRoot(), "..x")
	if err := (&Slot{Dir: dotted, top: top}).Reset(ctx, sha); err != nil {
		t.Fatalf("legitimate ..x entry refused: %v", err)
	}
	RemoveTree(ctx, top, dotted)
}

func TestSlotResetRefusesBusyDir(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	top, sha := auditFixtureRepo(t)
	ctx := context.Background()
	s, err := AcquireSlot(ctx, top, sha, "own")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Release()
	stubProcesses(t, func(string) ([]Proc, error) { return []Proc{{PID: 4242, Comm: "go"}}, nil })
	if err := s.Reset(ctx, sha); err == nil || !strings.Contains(err.Error(), "in use by pid 4242") {
		t.Fatalf("reset of a busy slot: %v", err)
	}
}
