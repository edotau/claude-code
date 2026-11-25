package worktree

import (
	"context"
	"os"
	"path/filepath"
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
