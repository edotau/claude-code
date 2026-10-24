package memory

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// storeFixture isolates the config dir and git's global config, and sets a HOSTILE inherited git env pointing at a
// decoy repo with staged work; every test asserts the decoy is untouched.
func storeFixture(t *testing.T) (store string, decoy Repo) {
	t.Helper()
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	decoy = Repo{Dir: filepath.Join(t.TempDir(), "decoy")}
	if err := decoy.Ensure(); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(decoy.Dir, "staged.md"), "staged, never committed\n")
	if _, err := decoy.run("add", "-A", "--", "staged.md", ".gitignore"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_DIR", filepath.Join(decoy.Dir, ".git"))
	t.Setenv("GIT_WORK_TREE", decoy.Dir)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(decoy.Dir, ".git", "index"))
	return StoreDir(), decoy
}

// assertDecoyUntouched: the hostile GIT_* env must never retarget a store operation at the decoy.
func assertDecoyUntouched(t *testing.T, decoy Repo) {
	t.Helper()
	if head, _ := decoy.Head(); head != "" {
		t.Fatalf("decoy gained a commit %s", head)
	}
	st, err := decoy.Status()
	if err != nil {
		t.Fatal(err)
	}
	if len(st) != 2 || st[0] != (StatusEntry{"A ", ".gitignore"}) || st[1] != (StatusEntry{"A ", "staged.md"}) {
		t.Fatalf("decoy index changed: %+v", st)
	}
}

func TestEnsureInitsNestedRepoWithGuards(t *testing.T) {
	store, decoy := storeFixture(t)
	r := StoreRepo()
	if r.Exists() {
		t.Fatal("store repo must not exist before Ensure")
	}
	if err := r.Ensure(); err != nil {
		t.Fatal(err)
	}
	if !r.Exists() {
		t.Fatal("Ensure did not create <store>/.git")
	}
	hookPath := filepath.Join(store, ".git", "hooks", "pre-push")
	hook, err := os.Stat(hookPath)
	if err != nil || hook.Mode()&0o100 == 0 {
		t.Fatalf("pre-push hook missing or not executable: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(store, ".gitignore"))
	if err != nil || !strings.Contains(string(data), "access.log") {
		t.Fatalf(".gitignore not seeded: %v", err)
	}
	if strings.Contains(string(data), "*.lock") {
		t.Error(".gitignore still lists *.lock; locks moved to state/, never tracked in the store")
	}
	// Idempotent: a user's own .gitignore edit survives a second Ensure.
	write(t, filepath.Join(store, ".gitignore"), "custom\n")
	if err := r.Ensure(); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(store, ".gitignore")); string(data) != "custom\n" {
		t.Fatalf(".gitignore overwritten: %q", data)
	}
	if head, _ := r.Head(); head != "" {
		t.Fatalf("fresh repo has HEAD %q", head)
	}
	if log, err := r.Log(5); err != nil || log != nil {
		t.Fatalf("Log on empty repo = %v, %v", log, err)
	}
	if remotes, err := r.Remotes(); err != nil || len(remotes) != 0 {
		t.Fatalf("fresh repo Remotes() = %v, %v; want none", remotes, err)
	}
	cmd := exec.Command("sh", hookPath)
	if err := cmd.Run(); err == nil {
		t.Fatal("pre-push hook must exit nonzero (refuses every push)")
	} else if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 1 {
		t.Fatalf("pre-push hook exit = %v, want exit 1", err)
	}
	assertDecoyUntouched(t, decoy)
}

func TestEnsureRefusesPointerFileAndRemotes(t *testing.T) {
	t.Parallel()
	store := filepath.Join(t.TempDir(), "memory")
	r := Repo{Dir: store}
	write(t, filepath.Join(store, ".git"), "gitdir: /elsewhere\n")
	if err := r.Ensure(); err == nil {
		t.Fatal("Ensure accepted a gitdir pointer file")
	}
	os.Remove(filepath.Join(store, ".git"))
	if err := r.Ensure(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.run("remote", "add", "origin", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	err := r.Ensure()
	if err == nil || !strings.Contains(err.Error(), "origin") {
		t.Fatalf("Ensure with a remote = %v, want error naming origin", err)
	}
}

func TestCommitStagesOnlyNamedPathsAndSkipsEmpty(t *testing.T) {
	store, decoy := storeFixture(t)
	r := StoreRepo()
	if err := r.Ensure(); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(store, "repo", "MEMORY.md"), "- fact\n")
	write(t, filepath.Join(store, "repo", "other.md"), "unrelated\n")
	sha, committed, err := r.Commit([]string{"repo/MEMORY.md"}, "settle", "")
	if err != nil || !committed || sha == "" {
		t.Fatalf("Commit = %q, %v, %v", sha, committed, err)
	}
	st, _ := r.Status()
	if len(st) != 2 || st[0].Path != ".gitignore" || st[1].Path != "repo/other.md" {
		t.Fatalf("unnamed paths must stay dirty: %+v", st)
	}
	if _, committed, err := r.Commit([]string{"repo/MEMORY.md"}, "again", ""); err != nil || committed {
		t.Fatalf("unchanged commit = %v, %v; want no empty commit", committed, err)
	}
	// Absent + untracked is dropped; absent + tracked stages a deletion.
	os.Remove(filepath.Join(store, "repo", "MEMORY.md"))
	if _, committed, err = r.Commit([]string{"repo/MEMORY.md", "never/existed.md"}, "delete", "body"); err != nil || !committed {
		t.Fatalf("deletion commit = %v, %v", committed, err)
	}
	log, err := r.Log(10)
	if err != nil || len(log) != 2 || !strings.HasSuffix(log[0], " delete") {
		t.Fatalf("Log = %v, %v", log, err)
	}
	if head, _ := r.Head(); head == "" {
		t.Fatal("Head empty after commits")
	}
	assertDecoyUntouched(t, decoy)
}

func TestCommitRejectsBadPathsAndSecretsByNameOnly(t *testing.T) {
	store, decoy := storeFixture(t)
	r := StoreRepo()
	if err := r.Ensure(); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"", ".", "..", "/abs/x.md", "a/../b.md", ".git/config", "../escape.md", "a//b.md"} {
		if _, _, err := r.Commit([]string{rel}, "s", ""); err == nil {
			t.Errorf("Commit accepted %q", rel)
		}
	}
	os.Symlink(filepath.Join(t.TempDir(), "outside"), filepath.Join(store, "link.md"))
	if _, _, err := r.Commit([]string{"link.md"}, "s", ""); err == nil {
		t.Error("Commit accepted a symlink")
	}
	const token = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdef"
	write(t, filepath.Join(store, "leak.md"), "token "+token+"\n")
	_, _, err := r.Commit([]string{"leak.md"}, "s", "")
	if err == nil || !strings.Contains(err.Error(), "leak.md") || strings.Contains(err.Error(), token) {
		t.Fatalf("secret rejection = %v; want path named, bytes absent", err)
	}
	write(t, filepath.Join(store, "prose.md"), "ask-before-changing-appearance-of-anything-here\n")
	if _, committed, err := r.Commit([]string{"prose.md"}, "s", ""); err != nil || !committed {
		t.Fatalf("kebab prose rejected: %v", err)
	}
	if head, _ := r.Head(); head == "" {
		t.Fatal("no commit after prose")
	}
	assertDecoyUntouched(t, decoy)
}

// Regression: checkRoot's commondir guard — a .git directory (not a pointer file, so Ensure's own
// foreign-worktree check never fires) whose "commondir" file redirects refs/objects to another repo
// must still be rejected before the store receives a byte.
func TestCheckRootRejectsSharedCommondir(t *testing.T) {
	t.Parallel()
	store := filepath.Join(t.TempDir(), "memory")
	other := filepath.Join(t.TempDir(), "other")
	if err := (Repo{Dir: other}).Ensure(); err != nil {
		t.Fatal(err)
	}
	r := Repo{Dir: store}
	gitDir := filepath.Join(store, ".git")
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := r.run("init", "--quiet", "--initial-branch=main"); err != nil {
		t.Fatal(err)
	}
	// Redirect this .git's commondir to the other repo's .git — refs/objects would be shared.
	rel, err := filepath.Rel(gitDir, filepath.Join(other, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{filepath.Join(other, ".git"), rel} {
		write(t, filepath.Join(gitDir, "commondir"), target+"\n")
		err = r.checkRoot(gitDir)
		if err == nil || !strings.Contains(err.Error(), "commondir") {
			t.Fatalf("checkRoot with commondir %q = %v, want a commondir-sharing error", target, err)
		}
	}
	os.Remove(filepath.Join(gitDir, "commondir"))
	if err := r.checkRoot(gitDir); err != nil {
		t.Fatalf("checkRoot without commondir = %v", err)
	}
}

func TestStatusParsesRenamesAndUntrackedInDirs(t *testing.T) {
	t.Parallel()
	store := filepath.Join(t.TempDir(), "memory")
	r := Repo{Dir: store}
	if err := r.Ensure(); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(store, "a.md"), "same body for rename detection, long enough to match\n")
	if _, _, err := r.Commit([]string{"a.md", ".gitignore"}, "base", ""); err != nil {
		t.Fatal(err)
	}
	os.Rename(filepath.Join(store, "a.md"), filepath.Join(store, "b.md"))
	write(t, filepath.Join(store, "deep", "dir", "c.md"), "c\n")
	if _, err := r.run("add", "-A", "--", "a.md", "b.md"); err != nil {
		t.Fatal(err)
	}
	st, err := r.Status()
	if err != nil {
		t.Fatal(err)
	}
	want := []StatusEntry{{"R ", "b.md"}, {"??", "deep/dir/c.md"}}
	if len(st) != len(want) || st[0] != want[0] || st[1] != want[1] {
		t.Fatalf("Status = %+v, want %+v", st, want)
	}
	if st, _ = r.Status("deep"); len(st) != 1 || st[0].Path != "deep/dir/c.md" {
		t.Fatalf("Status(pathspec) = %+v", st)
	}
}

func TestGitOutSanitizesEnv(t *testing.T) {
	_, decoy := storeFixture(t)
	other := filepath.Join(t.TempDir(), "other")
	if err := (Repo{Dir: other}).Ensure(); err != nil {
		t.Fatal(err)
	}
	top, err := gitOut(other, "rev-parse", "--show-toplevel")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := filepath.EvalSymlinks(top); got != mustEval(t, other) {
		t.Fatalf("gitOut followed the inherited GIT_DIR: %q", top)
	}
	if _, err := gitOut(t.TempDir(), "rev-parse", "--show-toplevel"); err == nil {
		t.Fatal("gitOut outside a repo must fail, not fall through to GIT_DIR")
	}
	assertDecoyUntouched(t, decoy)
}

func TestExitCodeDistinguishesDiff(t *testing.T) {
	t.Parallel()
	if exitCode(errors.New("plain")) != -1 {
		t.Fatal("non-exit error must be -1")
	}
}

func mustEval(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Store git calls run with -C: from a cwd inside the store, pathspecs and output stay relative to the store root.
func TestRepoRunIgnoresProcessCwd(t *testing.T) {
	store, _ := storeFixture(t)
	r := Repo{Dir: store}
	if err := r.Ensure(); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(store, "proj", "activeContext.md"), "x\n")
	t.Chdir(filepath.Join(store, "proj"))
	st, err := r.Status("proj/activeContext.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(st) != 1 || st[0].Path != "proj/activeContext.md" {
		t.Fatalf("Status from a store subdir = %+v, want proj/activeContext.md", st)
	}
}
