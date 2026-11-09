package bank

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/memstore"
)

func TestRotateSessionHistoryRetryDoesNotDuplicateArchive(t *testing.T) {
	dir := t.TempDir()
	date := time.Date(2026, time.November, 8, 12, 0, 0, 0, time.UTC)
	entries := make([]string, SessionHistoryMaxEntries+2)
	for i := range entries {
		entries[i] = fmt.Sprintf("## Session %02d\n\nDistinct body %02d.", i, i)
	}
	path := filepath.Join(dir, "sessionHistory.md")
	archive := filepath.Join(dir, "archive", "sessionHistory-2026-11.md")
	write(t, path, Rejoin("# Session history", entries))
	// Recreate an archive write that persisted before the source rewrite failed, plus one still-unarchived entry.
	write(t, archive, Rejoin("# Archived sessions", entries[SessionHistoryMaxEntries:SessionHistoryMaxEntries+1]))

	n, err := RotateSessionHistory(dir, date)
	if err != nil || n != 2 {
		t.Fatalf("retry rotation = %d, %v; want 2, nil", n, err)
	}
	_, archived := SplitHeadingBlocks(mustRead(t, archive))
	if want := entries[SessionHistoryMaxEntries:]; !slices.Equal(archived, want) {
		t.Fatalf("retry duplicated or lost archived entries:\n got: %q\nwant: %q", archived, want)
	}
	if got, want := mustRead(t, path), Rejoin("# Session history", entries[:SessionHistoryMaxEntries]); got != want {
		t.Fatalf("retained history:\n got: %q\nwant: %q", got, want)
	}
}

func TestRotateSessionHistoryReportsReadError(t *testing.T) {
	dir := t.TempDir()
	if n, err := RotateSessionHistory(dir, time.Now()); n != 0 || err != nil {
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
	n, err := RotateSessionHistory(dir, time.Now())
	if n != 0 || !errors.Is(err, want.Err) {
		t.Fatalf("unreadable history: got %d, %v; want wrapped %v", n, err, want.Err)
	}
}

// TestSettleBankLockLivesUnderStateNotStore: SettleBank's physical lock is BankLockPath() under state
// — never a dotfile beside the committed bank files.
func TestSettleBankLockLivesUnderStateNotStore(t *testing.T) {
	cfg := tempConfig(t)
	root := repoRoot(t, "proj")
	dir := memstore.Dir(root)
	if _, err := SettleBank(root, time.Now()); err != nil {
		t.Fatal(err)
	}
	want := memstore.BankLockPath()
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
