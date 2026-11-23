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
	for _, path := range []string{closed, open, claimed} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("dry run removed %s: %v", path, err)
		}
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("dry run created archive: %v", err)
	}
	if _, err := os.Stat(auditLogFile()); !os.IsNotExist(err) {
		t.Fatalf("dry run created audit log: %v", err)
	}

	changes, findings, err := Archive(context.Background(), root, false)
	if err != nil || len(findings) != 0 || len(changes) != 1 || changes[0].Plan != closed {
		t.Fatalf("archive: changes=%+v findings=%+v err=%v", changes, findings, err)
	}
	if _, err := os.Stat(closed); !os.IsNotExist(err) {
		t.Fatalf("closed plan still in home: %v", err)
	}
	if got := readFile(t, dest); got != "- [x] 1.1 Done\n" {
		t.Fatalf("archived plan = %q", got)
	}
	for _, path := range []string{open, claimed} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("ineligible plan moved %s: %v", path, err)
		}
	}
	if log := readFile(t, auditLogFile()); !strings.Contains(log, changes[0].Reopen) {
		t.Fatalf("archive missing reopen record: %s", log)
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
