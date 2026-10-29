package worktree

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// originClone builds an origin repo and a clone carrying one commit that is not pushed yet, so
// refs/remotes/origin/HEAD resolves and local HEAD is ahead of it.
func originClone(t *testing.T) string {
	t.Helper()
	origin := gitRepo(t)
	main := filepath.Join(t.TempDir(), "clone")
	cmd := exec.Command("git", "clone", "-q", origin, main)
	cmd.Env = gitEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone: %v: %s", err, out)
	}
	main, err := canonical(main)
	if err != nil {
		t.Fatal(err)
	}
	commitIn(t, main, "commit", "--allow-empty", "-q", "-m", "unpushed")
	return main
}

// rootFor gives main an isolated worktree root, like testRepo but for a caller-built main checkout.
func rootFor(t *testing.T, main string) *Repo {
	t.Helper()
	root, err := canonical(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &Repo{Main: main, Root: filepath.Join(root, "worktrees", filepath.Base(main))}
}

func revParse(t *testing.T, dir, ref string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "rev-parse", ref)
	cmd.Env = gitEnv()
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("rev-parse %s: %v", ref, err)
	}
	return strings.TrimSpace(string(out))
}

func writeBaseRef(t *testing.T, path, value string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"worktree":{"baseRef":"`+value+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCreateBaseRefFresh(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	main := originClone(t)
	writeBaseRef(t, filepath.Join(cfg, "settings.json"), "fresh")

	var log bytes.Buffer
	dest, err := Create(rootFor(t, main), "freshbase", CreateOptions{Log: &log})
	if err != nil {
		t.Fatal(err)
	}
	got := revParse(t, dest, "HEAD")
	if want := revParse(t, main, "refs/remotes/origin/HEAD"); got != want {
		t.Fatalf("fresh worktree HEAD = %s, want origin/HEAD %s (create warnings: %s)", got, want, log.String())
	}
	if local := revParse(t, main, "HEAD"); got == local {
		t.Fatal("fresh worktree HEAD equals the unpushed local HEAD; want the origin/HEAD commit")
	}
}

func TestCreateBaseRefHead(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	main := originClone(t)
	writeBaseRef(t, filepath.Join(cfg, "settings.json"), "head")

	var log bytes.Buffer
	dest, err := Create(rootFor(t, main), "headbase", CreateOptions{Log: &log})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := revParse(t, dest, "HEAD"), revParse(t, main, "HEAD"); got != want {
		t.Fatalf("head worktree HEAD = %s, want local HEAD %s (create warnings: %s)", got, want, log.String())
	}
}

func TestCreateBaseRefLocalSettingsOverride(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	main := originClone(t)
	writeBaseRef(t, filepath.Join(cfg, "settings.json"), "fresh")
	// The main checkout's settings.local.json is read last, so it overrides the user config file.
	writeBaseRef(t, filepath.Join(main, ".claude", "settings.local.json"), "head")

	var log bytes.Buffer
	dest, err := Create(rootFor(t, main), "override", CreateOptions{Log: &log})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := revParse(t, dest, "HEAD"), revParse(t, main, "HEAD"); got != want {
		t.Fatalf("override worktree HEAD = %s, want local HEAD %s (create warnings: %s)", got, want, log.String())
	}
}

// TestCleanFreshBranchDeletedAfterMerge is the regression test for the --no-track flag in
// lifecycle.go: a fresh-based worktree branch must have no upstream, so after its work is merged
// into main locally (nothing pushed), `git branch -d` — which checks mergedness against the
// branch's UPSTREAM when one is set, and that upstream is the still-behind origin/main — succeeds
// and Clean reports the branch deleted.
func TestCleanFreshBranchDeletedAfterMerge(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	main := originClone(t)
	writeBaseRef(t, filepath.Join(cfg, "settings.json"), "fresh")
	repo := rootFor(t, main)

	var log bytes.Buffer
	dest, err := Create(repo, "gone", CreateOptions{Log: &log})
	if err != nil {
		t.Fatal(err)
	}
	commitIn(t, dest, "commit", "--allow-empty", "-q", "-m", "work to merge")
	commitIn(t, main, "merge", "-q", "--no-edit", "worktree-gone")

	result, err := Clean(repo, "gone", false)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Removed {
		t.Fatalf("want removal, got refusal: %s", result.Reason)
	}
	if !result.BranchDeleted {
		t.Error("want the merged fresh-based branch deleted; a branch with an origin upstream survives `branch -d` until pushed")
	}
}

func TestSeedSymlinksVenvCopiesEnv(t *testing.T) {
	t.Parallel()
	repo := testRepo(t)
	main := repo.Main
	// .gitignore must be committed so the worktree checkout itself ignores the seeded paths.
	if err := os.WriteFile(filepath.Join(main, ".gitignore"), []byte(".venv\n.env\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commitIn(t, main, "add", ".gitignore")
	commitIn(t, main, "commit", "-q", "-m", "ignore seeded paths")
	if err := os.MkdirAll(filepath.Join(main, ".venv", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(main, ".venv", "bin", "activate"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(main, ".env"), []byte("TOKEN=secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(main, ".env"), 0o640); err != nil {
		t.Fatal(err)
	}

	var log bytes.Buffer
	dest, err := Create(repo, "seeded", CreateOptions{Log: &log})
	if err != nil {
		t.Fatal(err)
	}
	if log.Len() != 0 {
		t.Fatalf("seed warnings: %s", log.String())
	}

	info, err := os.Lstat(filepath.Join(dest, ".venv"))
	if err != nil {
		t.Fatalf(".venv missing: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf(".venv mode = %v, want a symlink", info.Mode())
	}
	if link, err := os.Readlink(filepath.Join(dest, ".venv")); err != nil || link != filepath.Join(main, ".venv") {
		t.Fatalf(".venv -> %q, err %v; want absolute %q", link, err, filepath.Join(main, ".venv"))
	}

	envInfo, err := os.Stat(filepath.Join(dest, ".env"))
	if err != nil {
		t.Fatalf(".env missing: %v", err)
	}
	if !envInfo.Mode().IsRegular() {
		t.Fatalf(".env mode = %v, want a regular file copy", envInfo.Mode())
	}
	if envInfo.Mode().Perm() != 0o640 {
		t.Fatalf(".env mode = %o, want 640 (source mode preserved)", envInfo.Mode().Perm())
	}
	if data, err := os.ReadFile(filepath.Join(dest, ".env")); err != nil || string(data) != "TOKEN=secret\n" {
		t.Fatalf(".env content = %q, err %v", data, err)
	}

	cmd := exec.Command("git", "-C", dest, "status", "--porcelain")
	cmd.Env = gitEnv()
	if out, err := cmd.Output(); err != nil || strings.TrimSpace(string(out)) != "" {
		t.Fatalf("worktree status = %q, err %v; seeded gitignored paths must leave the worktree clean", out, err)
	}
}

func TestSeedSkipsNonIgnoredEnv(t *testing.T) {
	t.Parallel()
	repo := testRepo(t)
	if err := os.WriteFile(filepath.Join(repo.Main, ".env"), []byte("TOKEN=secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dest, err := Create(repo, "noseed", CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dest, ".env")); !os.IsNotExist(err) {
		t.Fatalf(".env seeded (stat err = %v) even though it is not gitignored; a seeded tracked path would dirty the worktree", err)
	}
}

// TestSeedChecksDestIgnoreRules: a reused branch whose .gitignore lacks .env gets no copy, though main ignores it.
func TestSeedChecksDestIgnoreRules(t *testing.T) {
	t.Parallel()
	repo := testRepo(t)
	commitIn(t, repo.Main, "branch", "old-branch") // predates the ignore rule
	if err := os.WriteFile(filepath.Join(repo.Main, ".gitignore"), []byte(".env\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commitIn(t, repo.Main, "add", ".gitignore")
	commitIn(t, repo.Main, "commit", "-q", "-m", "ignore .env")
	if err := os.WriteFile(filepath.Join(repo.Main, ".env"), []byte("TOKEN=secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dest, err := Create(repo, "reused", CreateOptions{Branch: "old-branch"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dest, ".env")); !os.IsNotExist(err) {
		t.Fatalf(".env copied into a branch that does not ignore it (stat err = %v)", err)
	}
}
