package memory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Folding through a secondary checkout must not orphan the source's primary checkout.
func TestPersistenceAliasFoldViaAliasRetainsPrimary(t *testing.T) {
	e := newAliasEnv(t)
	secondary := gitRepo(t, canonTemp(t, t.TempDir()), "secondary")
	if err := addOriginAlias(e.db, secondary); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(e.db, "topic.md"), "source knowledge\n")
	if _, err := AliasRepo(AliasOptions{Repo: secondary, Into: e.ka}); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{e.a, e.b, secondary} {
		if got := Dir(root); got != e.da {
			t.Errorf("Dir(%q) = %q, want folded store %q", root, got, e.da)
		}
	}
	if pathExists(e.db) {
		t.Error("source store survived the fold")
	}
	if got := aliasRead(t, filepath.Join(e.da, "topic.md")); got != "source knowledge\n" {
		t.Errorf("folded topic = %q", got)
	}
}

// Contention must stop archiveFile before any read/rewrite, not merely lock its final write.
func TestPersistenceArchiveFileHonorsRememberLock(t *testing.T) {
	tempConfig(t)
	shortBankWait(t)
	root := repoRoot(t, "proj")
	path := filepath.Join(Dir(root), "conventions.md")
	body := "# Conventions\n\n- old one\n- old two\n"
	write(t, path, body)
	now := time.Now()
	err := WithBankLock(BankLockWait, func() error {
		_, err := archiveFile(path, 3, now)
		if !errors.Is(err, ErrBankBusy) {
			t.Errorf("archiveFile under Remember's bank lock = %v, want ErrBankBusy", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := aliasRead(t, path); got != body {
		t.Fatal("archiveFile rewrote the bank while its lock was held")
	}
	id, err := Remember(root, "feedback", "concurrent knowledge", now)
	if err != nil {
		t.Fatal(err)
	}
	n, err := archiveFile(path, 3, now)
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("fixture did not exercise the archive rewrite")
	}
	combined := aliasRead(t, path) + aliasRead(t, archivePath(filepath.Dir(path), filepath.Base(path), now))
	if !strings.Contains(combined, "id:"+id) {
		t.Error("Remember's entry was lost during archiving")
	}
}

// A failed run's retry deadline is independent of changes to either bank.
func TestPersistenceFailedDreamRetriesWithOldBanks(t *testing.T) {
	isolatedConfig(t)
	t.Setenv(UpdateChildEnv, "")
	t.Setenv(dreamChildEnv, "")
	root := repoRoot(t, "proj")
	if _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("agent unavailable")
	run := func(context.Context, string, string) (string, error) { return "stub", failure }
	if err := dream(dreamOptions{CWD: root, Run: run}); !errors.Is(err, failure) {
		t.Fatalf("Dream = %v, want agent failure", err)
	}
	now := time.Now()
	old := now.Add(-48 * time.Hour)
	for _, dir := range []string{Dir(root), HomeBankDir()} {
		for name := range templates() {
			path := filepath.Join(dir, name)
			write(t, path, templates()[name])
			if err := os.Chtimes(path, old, old); err != nil {
				t.Fatal(err)
			}
		}
		if got := NewestMtime(dir); got.After(old) {
			t.Fatalf("bank fixture has recent mtime: %v", got)
		}
	}
	if DreamDue(root, now.Add(dreamRetry-time.Minute)) {
		t.Error("failed dream is due before six-hour backoff")
	}
	if !DreamDue(root, now.Add(dreamRetry+time.Minute)) {
		t.Error("failed dream with two-day-old banks is not due after six hours")
	}
}

// Directories produce real read errors even when the test runs as root.
func TestPersistenceAliasDryRunRefusesReadErrors(t *testing.T) {
	for _, where := range []string{"destination", "archive"} {
		t.Run(where, func(t *testing.T) {
			e := newAliasEnv(t)
			name := "activeContext.md"
			write(t, filepath.Join(e.db, name), "source\n")
			blocked := filepath.Join(e.da, name)
			if where == "archive" {
				write(t, blocked, "target\n")
				blocked = filepath.Join(e.da, "archive", "merged-"+e.kb+"-"+name)
			}
			if err := os.MkdirAll(blocked, 0o700); err != nil {
				t.Fatal(err)
			}
			before := aliasTreeHash(t, StoreDir())
			_, err := alias(t, e, true)
			var readErr *os.PathError
			if !errors.As(err, &readErr) {
				t.Fatalf("dry-run with unreadable %s = %v, want read error", where, err)
			}
			if readErr.Path != blocked {
				t.Errorf("error path = %q, want %q", readErr.Path, blocked)
			}
			if after := aliasTreeHash(t, StoreDir()); after != before {
				t.Error("dry-run changed the store while refusing a read error")
			}
		})
	}
}
