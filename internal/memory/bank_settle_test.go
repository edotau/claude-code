package memory

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestRotateSessionHistoryRetryDoesNotDuplicateArchive(t *testing.T) {
	dir := t.TempDir()
	date := time.Date(2026, time.November, 8, 12, 0, 0, 0, time.UTC)
	entries := make([]string, sessionHistoryMaxEntries+2)
	for i := range entries {
		entries[i] = fmt.Sprintf("## Session %02d\n\nDistinct body %02d.", i, i)
	}
	path := filepath.Join(dir, "sessionHistory.md")
	archive := filepath.Join(dir, "archive", "sessionHistory-2026-11.md")
	write(t, path, rejoin("# Session history", entries))
	// Recreate an archive write that persisted before the source rewrite failed, plus one still-unarchived entry.
	write(t, archive, rejoin("# Archived sessions", entries[sessionHistoryMaxEntries:sessionHistoryMaxEntries+1]))

	n, err := rotateSessionHistory(dir, date)
	if err != nil || n != 2 {
		t.Fatalf("retry rotation = %d, %v; want 2, nil", n, err)
	}
	_, archived := splitHeadingBlocks(mustRead(t, archive))
	if want := entries[sessionHistoryMaxEntries:]; !slices.Equal(archived, want) {
		t.Fatalf("retry duplicated or lost archived entries:\n got: %q\nwant: %q", archived, want)
	}
	if got, want := mustRead(t, path), rejoin("# Session history", entries[:sessionHistoryMaxEntries]); got != want {
		t.Fatalf("retained history:\n got: %q\nwant: %q", got, want)
	}
}

func TestRotateSessionHistoryReportsReadError(t *testing.T) {
	dir := t.TempDir()
	if n, err := rotateSessionHistory(dir, time.Now()); n != 0 || err != nil {
		t.Fatalf("missing history: got %d, %v", n, err)
	}
	path := filepath.Join(dir, "sessionHistory.md")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	_, readErr := os.ReadFile(path)
	var want *os.PathError
	if !errors.As(readErr, &want) {
		t.Fatalf("fixture did not produce a read error: %v", readErr)
	}
	n, err := rotateSessionHistory(dir, time.Now())
	if n != 0 || !errors.Is(err, want.Err) {
		t.Fatalf("unreadable history: got %d, %v; want wrapped %v", n, err, want.Err)
	}
}

// TestSettleBankLockLivesUnderStateNotStore: settleBank's physical lock is BankLockPath() under state
// — never a dotfile beside the committed bank files.
func TestSettleBankLockLivesUnderStateNotStore(t *testing.T) {
	cfg := tempConfig(t)
	root := repoRoot(t, "proj")
	dir := Dir(root)
	if _, err := settleBank(root, time.Now()); err != nil {
		t.Fatal(err)
	}
	want := BankLockPath()
	if !strings.HasPrefix(want, filepath.Join(cfg, "state")) {
		t.Fatalf("bank lock %s is not under state", want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("lock file missing at %s: %v", want, err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".postupdate.lock")); err == nil {
		t.Error("a legacy-shaped lock file was written beside the store")
	}
	if _, err := os.Stat(filepath.Join(dir, ".postupdate")); err == nil {
		t.Error("a legacy-shaped lock file was written beside the store")
	}
}

// fakeCredential builds a credential-shaped string at runtime; it is not a real secret.
func fakeCredential() string { return "sk-" + "ant-api03-" + strings.Repeat("x9Y_", 6) }

// TestSettleBankIgnoresAnotherReposCredentialFile: Given a credential-shaped file in another repo's memory dir, When
// this repo settles, Then it commits and leaves that file alone; the same file in the settled dir is still refused.
func TestSettleBankIgnoresAnotherReposCredentialFile(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "proj")
	if err := StoreRepo().Ensure(); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(StoreDir(), "otherrepo", "note.md")
	write(t, other, "key: "+fakeCredential()+"\n")

	if _, err := settleBank(root, time.Now()); err != nil {
		t.Fatalf("an unrelated repo's credential-shaped file failed this repo's settle: %v", err)
	}
	st, err := StoreRepo().Status("otherrepo")
	if err != nil || len(st) != 1 || st[0].Path != "otherrepo/note.md" {
		t.Fatalf("other repo's file must stay uncommitted: %+v, %v", st, err)
	}
	if st, _ := StoreRepo().Status(filepath.Base(Dir(root))); len(st) != 0 {
		t.Fatalf("this repo's settle left its own dir dirty: %+v", st)
	}

	write(t, filepath.Join(Dir(root), "leak.md"), "key: "+fakeCredential()+"\n")
	_, err = settleBank(root, time.Now())
	if err == nil || !strings.Contains(err.Error(), "credential-shaped content") {
		t.Fatalf("a credential-shaped file in the settled dir must be refused, got %v", err)
	}
	if strings.Contains(err.Error(), fakeCredential()) {
		t.Fatal("the refusal leaked the credential")
	}
}
