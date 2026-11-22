package plans

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestMovePlanArchiveCrossDevice(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	orig := rename
	rename = func(string, string) error { return &os.LinkError{Op: "rename", Err: syscall.EXDEV} }
	t.Cleanup(func() { rename = orig })
	dir := t.TempDir()
	from, to := filepath.Join(dir, "2026-01-01-x.md"), filepath.Join(dir, "archive", "2026-01-01-x.md")
	if err := os.WriteFile(from, []byte("# x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mtime := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(from, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	if err := movePlan(Change{Plan: from, Action: "archive"}, to); err != nil {
		t.Fatalf("movePlan: %v", err)
	}
	if _, err := os.Stat(from); !os.IsNotExist(err) {
		t.Fatalf("source still present: %v", err)
	}
	info, err := os.Stat(to)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(to); string(b) != "# x\n" || !info.ModTime().Equal(mtime) {
		t.Fatalf("copy = %q mtime %v, want %q %v", b, info.ModTime(), "# x\n", mtime)
	}
}
