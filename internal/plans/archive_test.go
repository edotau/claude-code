package plans

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestArchiveEligibilityAndDryRun(t *testing.T) {
	hermetic(t)
	root := t.TempDir()
	home := Home(root)
	closed := filepath.Join(home, "2026-10-05-closed.md")
	open := filepath.Join(home, "2026-10-05-open.md")
	claimed := filepath.Join(home, "2026-10-05-claimed.md")
	writeFile(t, closed, "- [x] 1.1 Done\n")
	writeFile(t, open, "- [ ] 1.1 Pending\n")
	writeFile(t, claimed, "- [x] 1.1 Done <!-- working: alice -->\n")
	dest := filepath.Join(archiveDir(root), filepath.Base(closed))

	dry, findings, err := Archive(context.Background(), root, true)
	if err != nil || len(findings) != 0 || len(dry) != 1 || dry[0].Plan != closed || dry[0].Action != "archive" {
		t.Fatalf("dry run: changes=%+v findings=%+v err=%v", dry, findings, err)
	}
	wantFiles(t, "dry run", []string{closed, open, claimed}, []string{dest, auditLogFile()})

	changes, findings, err := Archive(context.Background(), root, false)
	if err != nil || len(findings) != 0 || len(changes) != 1 || changes[0].Plan != closed {
		t.Fatalf("archive: changes=%+v findings=%+v err=%v", changes, findings, err)
	}
	wantFiles(t, "archive", []string{open, claimed, dest}, []string{closed})
	if got := readFile(t, dest); got != "- [x] 1.1 Done\n" {
		t.Fatalf("archived plan = %q", got)
	}
	if log := readFile(t, auditLogFile()); !strings.Contains(log, changes[0].Reopen) {
		t.Fatalf("archive missing reopen record: %s", log)
	}
}

// wantFiles fails unless every present path exists and every absent one does not.
func wantFiles(t *testing.T, step string, present, absent []string) {
	t.Helper()
	for _, path := range present {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s: %s should exist: %v", step, path, err)
		}
	}
	for _, path := range absent {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s: %s should not exist: %v", step, path, err)
		}
	}
}

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
