package memory

import (
	"os"
	"os/exec"
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
	if StoreDir() != filepath.Join(cfg, "memory") || harnessStoreDir() != filepath.Join(cfg, "memory", filepath.Base(cfg)) {
		t.Fatal("store paths moved")
	}
	if JournalDir() != filepath.Join(StateDir(), "journal") || knowledgeDir("/x") != "/x/knowledge" {
		t.Fatal("journal/knowledge paths moved")
	}
	if archiveDir("/x") != "/x/archive" || HandoffsDir("/x") != "/x/handoffs" {
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
		if rel, ok := insideStore(p); rel != want.rel || ok != want.ok {
			t.Errorf("insideStore(%q) = %q, %v; want %q, %v", p, rel, ok, want.rel, want.ok)
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
	if _, ok := insideStore(filepath.Join(StoreDir(), "escape.md")); ok {
		t.Error("insideStore accepted a symlink escaping the store")
	}
}

func TestMemoryRootHonorsHarnessWorktreePointer(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	worktree := t.TempDir()
	pointer := "gitdir: " + filepath.Join(cfg, ".git", "worktrees", "x") + "\n"
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte(pointer), 0o600); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	if out, err := exec.Command("git", "-C", other, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	t.Chdir(other)
	t.Setenv("CLAUDE_PROJECT_DIR", worktree)
	if got := Root(""); got != worktree {
		t.Fatalf("Root(\"\") = %q, want harness worktree %q (not cwd %q)", got, worktree, other)
	}
}

// The inverse: CLAUDE_CONFIG_DIR is itself a linked worktree, so its main checkout is not the config dir.
func TestMemoryRootHonorsConfigDirWorktree(t *testing.T) {
	main := t.TempDir()
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	pointer := "gitdir: " + filepath.Join(main, ".git", "worktrees", "x") + "\n"
	if err := os.WriteFile(filepath.Join(cfg, ".git"), []byte(pointer), 0o600); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	if out, err := exec.Command("git", "-C", other, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	t.Chdir(other)
	t.Setenv("CLAUDE_PROJECT_DIR", cfg)
	if got := Root(""); got != cfg {
		t.Fatalf("Root(\"\") = %q, want config worktree %q (not cwd %q)", got, cfg, other)
	}
}

// A CLAUDE_PROJECT_DIR that is not a repo (a launcher stamping $HOME) must not win over the cwd's toplevel.
func TestMemoryRootIgnoresNonRepoProjectDir(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(repo, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Skipf("git init: %v %s", err, out)
	}
	t.Chdir(filepath.Join(repo, "sub"))
	want, _ := filepath.EvalSymlinks(repo)

	t.Setenv("CLAUDE_PROJECT_DIR", filepath.Dir(repo)) // the non-repo parent
	if got, _ := filepath.EvalSymlinks(Root("")); got != want {
		t.Errorf("non-repo CLAUDE_PROJECT_DIR won: got %s want %s", got, want)
	}
	t.Setenv("CLAUDE_PROJECT_DIR", repo) // a real repo root is honored
	if got, _ := filepath.EvalSymlinks(Root("")); got != want {
		t.Errorf("repo CLAUDE_PROJECT_DIR not honored: got %s want %s", got, want)
	}
	t.Setenv("CLAUDE_PROJECT_DIR", cfg) // the config dir itself is the global bank's root
	if got := Root(""); got != cfg {
		t.Errorf("config dir not honored: got %s", got)
	}
}
