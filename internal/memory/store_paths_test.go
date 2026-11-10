package memory

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/edotau/claude-code/internal/vault"
)

func TestDirUnreadableOriginNeverFallsBackToHome(t *testing.T) {
	tempConfig(t)
	resetVaultRetry(t)
	repo := gitRepo(t, t.TempDir(), "project")
	if _, err := EnsureRepoDir(repo); err != nil {
		t.Fatal(err)
	}
	write(t, vault.Path(), "not a vault")
	if entries, err := RepoDirs(); err == nil {
		t.Fatalf("RepoDirs = %+v, nil; want an unreadable-vault error without a catalog fallback", entries)
	}

	want := filepath.Join(StateDir(), "unresolved-store")
	if got := Dir(repo); got != want {
		t.Fatalf("Dir(repo) = %q; want inert path %q, never home bank %q", got, want, HomeBankDir())
	}
}

func TestStorePaths(t *testing.T) {
	cfg := tempConfig(t)
	// No origin remote here, so the harness key falls back to the config dir's own basename, not ".claude".
	if StoreDir() != filepath.Join(cfg, "memory") || HarnessStoreDir() != filepath.Join(cfg, "memory", filepath.Base(cfg)) {
		t.Fatal("store paths moved")
	}
	if JournalDir() != filepath.Join(StateDir(), "journal") || KnowledgeDir("/x") != "/x/knowledge" {
		t.Fatal("journal/knowledge paths moved")
	}
	if ArchiveDir("/x") != "/x/archive" || HandoffsDir("/x") != "/x/handoffs" {
		t.Fatal("archive/handoffs paths moved")
	}
}

func TestInsideStore(t *testing.T) {
	cfg := tempConfig(t)
	cases := map[string]struct {
		rel string
		ok  bool
	}{
		filepath.Join(StoreDir(), "repo", "MEMORY.md"): {"repo/MEMORY.md", true},
		filepath.Join(StoreDir(), ".git", "config"):    {"", false},
		filepath.Join(StoreDir(), ".gitx"):             {".gitx", true},
		filepath.Join(cfg, "memory-other", "x.md"):     {"", false},
		filepath.Join(cfg, "docs", "x.md"):             {"", false},
		StoreDir():                                     {".", true},
	}
	for p, want := range cases {
		if rel, ok := InsideStore(p); rel != want.rel || ok != want.ok {
			t.Errorf("InsideStore(%q) = %q, %v; want %q, %v", p, rel, ok, want.rel, want.ok)
		}
	}
}

// A symlink escaping the store is outside it even though its lexical path is inside.
func TestInsideStoreRejectsEscapingSymlink(t *testing.T) {
	tempConfig(t)
	if err := os.MkdirAll(StoreDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.md")
	os.WriteFile(outside, []byte("x"), 0o600)
	if err := os.Symlink(outside, filepath.Join(StoreDir(), "escape.md")); err != nil {
		t.Fatal(err)
	}
	if _, ok := InsideStore(filepath.Join(StoreDir(), "escape.md")); ok {
		t.Error("InsideStore accepted a symlink escaping the store")
	}
}
