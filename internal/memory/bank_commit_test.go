package memory

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/platform/hosttest"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/textutil"
)

// captureStderr redirects os.Stderr to a file until the test ends; the returned func reads what was written so far.
func captureStderr(t *testing.T) func() string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stderr
	os.Stderr = f
	t.Cleanup(func() { os.Stderr = original; _ = f.Close() })
	return func() string { return mustRead(t, f.Name()) }
}

func readStore(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(paths.MemoryStoreDir(), rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}

// commitLocked runs one batch under the writable bank lock, the shape every caller uses.
func commitLocked(t *testing.T, subject string, changes []storeChange) (commitReceipt, error) {
	t.Helper()
	var rec commitReceipt
	err := WithBankLock(BankLockWait, func() error {
		var err error
		rec, err = commitChangesLocked(subject, changes)
		return err
	})
	return rec, err
}

// assertNoopBatch: a second identical batch changes no bytes, so there is no commit.
func assertNoopBatch(t *testing.T, first commitReceipt) {
	t.Helper()
	rec2, err := commitLocked(t, "import", []storeChange{{Rel: "repo/MEMORY.md", Data: []byte("- a\n")}})
	if err != nil || rec2.Commit != "" || len(rec2.Skipped) != 1 {
		t.Fatalf("no-op batch = %+v, %v", rec2, err)
	}
	if head, _ := StoreRepo().head(); head != first.Commit {
		t.Fatalf("empty commit created: %s != %s", head, first.Commit)
	}
}

func TestCommitChangesLockedFreshImportCommits(t *testing.T) {
	store, decoy := storeFixture(t)
	rec, err := commitLocked(t, "import", []storeChange{
		{Rel: "repo/MEMORY.md", Data: []byte("- a\n")},
		{Rel: "repo/activeContext.md", Data: []byte("# Active\n")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Applied) != 2 || rec.Commit == "" || len(rec.Divergent)+len(rec.Skipped) != 0 {
		t.Fatalf("receipt = %+v", rec)
	}
	if !strings.Contains(strings.Join(rec.Preexisting, ","), ".gitignore") {
		t.Fatalf("seeded .gitignore must be swept as preexisting: %+v", rec.Preexisting)
	}
	st, _ := StoreRepo().Status()
	if len(st) != 0 {
		t.Fatalf("store dirty after commit: %+v", st)
	}
	if _, err := os.Stat(filepath.Join(store, ".git", "hooks", "pre-push")); err != nil {
		t.Fatal(err)
	}
	assertNoopBatch(t, rec)
	assertDecoyUntouched(t, decoy)
}

// Delete: absent is skipped, present is removed and committed.
func TestCommitChangesLockedDeleteRules(t *testing.T) {
	store, _ := storeFixture(t)
	if _, err := commitLocked(t, "seed", []storeChange{{Rel: "r/new.md", Data: []byte("new\n")}}); err != nil {
		t.Fatal(err)
	}
	rec, err := commitLocked(t, "delete", []storeChange{
		{Rel: "r/gone.md", Delete: true},
		{Rel: "r/new.md", Delete: true},
	})
	if err != nil || strings.Join(rec.Skipped, ",") != "r/gone.md" || strings.Join(rec.Applied, ",") != "r/new.md" {
		t.Fatalf("delete batch = %+v, %v", rec, err)
	}
	if _, err := os.Stat(filepath.Join(store, "r", "new.md")); !os.IsNotExist(err) {
		t.Fatal("r/new.md not removed")
	}
	if out, _ := StoreRepo().run("ls-files", "r/new.md"); out != "" {
		t.Fatal("deletion not committed")
	}
}

// Regression: validateRel refuses an escaping rel and a symlinked ancestor directory, not just a symlinked leaf.
func TestCommitChangesLockedRefusesEscapeAndSymlinkAncestor(t *testing.T) {
	hosttest.SkipOnWindows(t, "sh scripts, symlinks or errno")
	store, _ := storeFixture(t)
	if _, err := commitLocked(t, "x", []storeChange{{Rel: "../out.md", Data: []byte("a")}}); err == nil {
		t.Fatal("escaping rel accepted")
	}
	if StoreRepo().Exists() {
		t.Fatal("rejected batch initialized the repo")
	}
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(store, "repo")); err != nil {
		t.Fatal(err)
	}
	if _, err := commitLocked(t, "escape", []storeChange{{Rel: "repo/sub/MEMORY.md", Data: []byte("x\n")}}); err == nil {
		t.Fatal("commit accepted a write through a symlinked ancestor directory")
	}
	if _, err := os.Stat(filepath.Join(outside, "sub", "MEMORY.md")); !os.IsNotExist(err) {
		t.Fatal("bytes escaped through the symlinked ancestor")
	}
}

func TestCommitChangesLockedSkipsSymlinkPreexisting(t *testing.T) {
	hosttest.SkipOnWindows(t, "sh scripts, symlinks or errno")
	store, _ := storeFixture(t)
	if _, err := commitLocked(t, "seed", []storeChange{{Rel: "r/a.md", Data: []byte("a\n")}}); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.md")
	os.WriteFile(outside, []byte("secret-ish"), 0o600)
	if err := os.Symlink(outside, filepath.Join(store, "r", "link.md")); err != nil {
		t.Fatal(err)
	}
	rec, err := commitLocked(t, "settle", []storeChange{{Rel: "r/a.md", Data: []byte("b\n")}})
	if err != nil || len(rec.Preexisting) != 0 {
		t.Fatalf("symlink swept: %+v, %v", rec, err)
	}
	if out, _ := StoreRepo().run("ls-files", "r/link.md"); out != "" {
		t.Fatal("symlink was committed")
	}
}

// batchLocked runs one batch under the bank lock like commitLocked, but without a *testing.T.
func batchLocked(subject string, changes []storeChange) (rec commitReceipt, err error) {
	err = WithBankLock(BankLockWait, func() error {
		var err error
		rec, err = commitChangesLocked(subject, changes)
		return err
	})
	return rec, err
}

// A credential-shaped file outside the batch's dirs is named once on stderr and never swept; inside, it refuses.
func TestCommitChangesLockedSkipsAnotherReposCredentialFile(t *testing.T) {
	placeholder := useCredentialPlaceholder(t)
	storeFixture(t)
	write(t, filepath.Join(paths.MemoryStoreDir(), "other", "note.md"), "key: "+placeholder+"\n")
	readStderr := captureStderr(t)
	rec, err := commitLocked(t, "import", []storeChange{{Rel: "r/a.md", Data: []byte("a\n")}})
	stderr := readStderr()
	if err != nil {
		t.Fatalf("an unrelated credential-shaped file failed the batch: %v", err)
	}
	if slices.Contains(rec.Preexisting, "other/note.md") {
		t.Fatalf("credential-shaped file swept into the batch: %+v", rec.Preexisting)
	}
	if n := strings.Count(stderr, "other/note.md"); n != 1 || strings.Contains(stderr, placeholder) {
		t.Fatalf("stderr must name the path once and no secret: %q", stderr)
	}
	if st, _ := StoreRepo().Status("other"); len(st) != 1 {
		t.Fatalf("the skipped file must stay uncommitted: %+v", st)
	}

	write(t, filepath.Join(paths.MemoryStoreDir(), "r", "leak.md"), "key: "+placeholder+"\n")
	if _, err := commitLocked(t, "import", []storeChange{{Rel: "r/b.md", Data: []byte("b\n")}}); err == nil ||
		!strings.Contains(err.Error(), "credential-shaped content") {
		t.Fatalf("a credential-shaped file in the batch's dir must be refused, got %v", err)
	}
}

// storeFixture isolates config and sets a hostile inherited git env at a decoy repo; tests assert it stays untouched.
func storeFixture(t *testing.T) (store string, decoy storeRepo) {
	t.Helper()
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	decoy = storeRepo{Dir: filepath.Join(t.TempDir(), "decoy")}
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
	return paths.MemoryStoreDir(), decoy
}

// assertDecoyUntouched: the hostile GIT_* env must never retarget a store operation at the decoy.
func assertDecoyUntouched(t *testing.T, decoy storeRepo) {
	t.Helper()
	if head, _ := decoy.head(); head != "" {
		t.Fatalf("decoy gained a commit %s", head)
	}
	st, err := decoy.Status()
	if err != nil {
		t.Fatal(err)
	}
	if len(st) != 2 || st[0] != (statusEntry{"A ", ".gitignore"}) || st[1] != (statusEntry{"A ", "staged.md"}) {
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
	if err != nil || runtime.GOOS != "windows" && hook.Mode()&0o100 == 0 { // Windows has no exec bit
		t.Fatalf("pre-push hook missing or not executable: %v", err)
	}
	assertGitignoreSeeded(t, store)
	// Idempotent: a user's own .gitignore edit survives a second Ensure.
	write(t, filepath.Join(store, ".gitignore"), "custom\n")
	if err := r.Ensure(); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(store, ".gitignore")); string(data) != "custom\n" {
		t.Fatalf(".gitignore overwritten: %q", data)
	}
	assertFreshRepoEmpty(t, r)
	assertPrePushRefuses(t, hookPath)
	assertDecoyUntouched(t, decoy)
}

func TestEnsureRefreshesStaleHarnessGitignore(t *testing.T) {
	store, _ := storeFixture(t)
	r := StoreRepo()
	if err := r.Ensure(); err != nil {
		t.Fatal(err)
	}
	wsl := gitignoreHeader + "\n*.lock\n.postharvest*\n.promote.lock\n.archive*\nmine/\n" // mine/: a user line
	write(t, filepath.Join(store, ".gitignore"), wsl)
	if err := r.Ensure(); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(store, ".gitignore")); string(data) != storeGitignore+"mine/\n" {
		t.Fatalf(".gitignore not refreshed with the user line kept: %q", data)
	}
}

// assertGitignoreSeeded checks Ensure wrote the store's .gitignore with access.log and without *.lock.
func assertGitignoreSeeded(t *testing.T, store string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(store, ".gitignore"))
	if err != nil || !strings.Contains(string(data), "access.log") {
		t.Fatalf(".gitignore not seeded: %v", err)
	}
	if strings.Contains(string(data), "*.lock") {
		t.Error(".gitignore still lists *.lock; locks moved to state/, never tracked in the store")
	}
}

// assertFreshRepoEmpty checks a just-ensured repo has no HEAD, no log and no remotes.
func assertFreshRepoEmpty(t *testing.T, r storeRepo) {
	t.Helper()
	if head, _ := r.head(); head != "" {
		t.Fatalf("fresh repo has HEAD %q", head)
	}
	if remotes, err := r.remotes(); err != nil || len(remotes) != 0 {
		t.Fatalf("fresh repo remotes() = %v, %v; want none", remotes, err)
	}
}

// assertPrePushRefuses runs the hook and requires exit status 1.
func assertPrePushRefuses(t *testing.T, hookPath string) {
	hosttest.SkipOnWindows(t, "sh scripts, symlinks or errno")
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("running the pre-push hook needs sh on PATH")
	}
	cmd := exec.Command("sh", hookPath)
	if err := cmd.Run(); err == nil {
		t.Fatal("pre-push hook must exit nonzero (refuses every push)")
	} else if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 1 {
		t.Fatalf("pre-push hook exit = %v, want exit 1", err)
	}
}

func TestEnsureRefusesPointerFileAndRemotes(t *testing.T) {
	t.Parallel()
	store := filepath.Join(t.TempDir(), "memory")
	r := storeRepo{Dir: store}
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
	assertDecoyUntouched(t, decoy)
}

// The sha parsed from commit's output and the ref read from disk agree with what git rev-parse reports.
func TestCommitShaMatchesGitHead(t *testing.T) {
	store, _ := storeFixture(t)
	r := StoreRepo()
	if err := r.Ensure(); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(store, "repo", "MEMORY.md"), "- fact\n")
	sha, _, err := r.Commit([]string{"repo/MEMORY.md"}, "settle", "")
	want, _ := r.head()
	full, ok := r.headSHA()
	if err != nil || sha != want || !ok || !strings.HasPrefix(full, sha) {
		t.Fatalf("sha %q (%v), git %q, headSHA %q %v", sha, err, want, full, ok)
	}
}

// Absent + untracked is dropped; absent + tracked stages a deletion.
func TestCommitDropsUntrackedAndStagesDeletion(t *testing.T) {
	store, decoy := storeFixture(t)
	r := StoreRepo()
	if err := r.Ensure(); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(store, "repo", "MEMORY.md"), "- fact\n")
	if _, committed, err := r.Commit([]string{"repo/MEMORY.md"}, "settle", ""); err != nil || !committed {
		t.Fatalf("seed commit = %v, %v", committed, err)
	}
	os.Remove(filepath.Join(store, "repo", "MEMORY.md"))
	if _, committed, err := r.Commit([]string{"repo/MEMORY.md", "never/existed.md"}, "delete", "body"); err != nil || !committed {
		t.Fatalf("deletion commit = %v, %v", committed, err)
	}
	log, err := r.run("log", "--format=%s")
	if err != nil || strings.Count(log, "\n") != 1 || !strings.HasPrefix(log, "delete\n") {
		t.Fatalf("log = %q, %v", log, err)
	}
	if head, _ := r.head(); head == "" {
		t.Fatal("Head empty after commits")
	}
	assertDecoyUntouched(t, decoy)
}

func TestCommitRejectsBadPathsAndSymlinks(t *testing.T) {
	hosttest.SkipOnWindows(t, "sh scripts, symlinks or errno")
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
	if err := os.Symlink(filepath.Join(t.TempDir(), "outside"), filepath.Join(store, "link.md")); err != nil {
		assertDecoyUntouched(t, decoy)
		t.Skipf("symlinks unavailable here: %v", err)
	}
	if _, _, err := r.Commit([]string{"link.md"}, "s", ""); err == nil {
		t.Error("Commit accepted a symlink")
	}
	assertDecoyUntouched(t, decoy)
}

func TestCommitRejectsSecretsByNameOnly(t *testing.T) {
	store, decoy := storeFixture(t)
	r := StoreRepo()
	if err := r.Ensure(); err != nil {
		t.Fatal(err)
	}
	t.Run("detected placeholder", func(t *testing.T) {
		token := useCredentialPlaceholder(t)
		write(t, filepath.Join(store, "leak.md"), "token "+token+"\n")
		_, _, err := r.Commit([]string{"leak.md"}, "s", "")
		if err == nil || !strings.Contains(err.Error(), "leak.md") || strings.Contains(err.Error(), token) {
			t.Fatalf("secret rejection = %v; want path named, bytes absent", err)
		}
	})
	// The placeholder seam is restored before the real detector sees ordinary prose.
	write(t, filepath.Join(store, "prose.md"), "kebab-case! plain prose\n")
	if _, committed, err := r.Commit([]string{"prose.md"}, "s", ""); err != nil || !committed {
		t.Fatalf("kebab prose rejected: %v", err)
	}
	if head, _ := r.head(); head == "" {
		t.Fatal("no commit after prose")
	}
	assertDecoyUntouched(t, decoy)
}

// Regression: checkRoot rejects a .git directory whose "commondir" redirects objects to another repo.
func TestCheckRootRejectsSharedCommondir(t *testing.T) {
	t.Parallel()
	store := filepath.Join(t.TempDir(), "memory")
	other := filepath.Join(t.TempDir(), "other")
	if err := (storeRepo{Dir: other}).Ensure(); err != nil {
		t.Fatal(err)
	}
	r := storeRepo{Dir: store}
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
	r := storeRepo{Dir: store}
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
	want := []statusEntry{{"R ", "b.md"}, {"D ", "a.md"}, {"??", "deep/dir/c.md"}}
	if !slices.Equal(st, want) {
		t.Fatalf("Status = %+v, want %+v", st, want)
	}
	if st, _ = r.Status("deep"); len(st) != 1 || st[0].Path != "deep/dir/c.md" {
		t.Fatalf("Status(pathspec) = %+v", st)
	}
}

func TestGitOutSanitizesEnv(t *testing.T) {
	_, decoy := storeFixture(t)
	other := filepath.Join(t.TempDir(), "other")
	if err := (storeRepo{Dir: other}).Ensure(); err != nil {
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
	r := storeRepo{Dir: store}
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

func TestStoreGitProbesBounded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nexec \""+sleep+"\" 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	oldTimeout := gitTimeout
	gitTimeout = 50 * time.Millisecond
	t.Cleanup(func() { gitTimeout = oldTimeout })

	t.Run("git toplevel", func(t *testing.T) {
		started := time.Now()
		if got := GitToplevel(t.TempDir()); got != "" {
			t.Errorf("timed-out toplevel probe = %q, want empty", got)
		}
		if elapsed := time.Since(started); elapsed >= time.Second {
			t.Errorf("toplevel probe took %v with timeout %v", elapsed, gitTimeout)
		}
	})
}

// TestHeadDistinguishesUnbornFromFailure: an unborn HEAD is ("", nil); any other git failure is returned.
func TestHeadDistinguishesUnbornFromFailure(t *testing.T) {
	storeFixture(t)
	r := storeRepo{Dir: filepath.Join(t.TempDir(), "repo")}
	if err := r.Ensure(); err != nil {
		t.Fatal(err)
	}
	if head, err := r.head(); err != nil || head != "" {
		t.Fatalf("unborn Head = %q, %v, want \"\", nil", head, err)
	}
	if _, err := (storeRepo{Dir: filepath.Join(t.TempDir(), "missing")}).head(); err == nil {
		t.Fatal("Head on a non-repo = nil error, want the git failure")
	}
}

const memoryCredentialPlaceholder = "not a credential!"

// useCredentialPlaceholder tests consumers through existing regex seams, not vendor token shapes.
func useCredentialPlaceholder(t *testing.T) string {
	t.Helper()
	// Keep the derived start-byte cache on the production pattern, regardless of test order.
	_ = textutil.LooksSecret("ordinary prose!")
	oldRE, oldScan := textutil.CredentialRE, credentialScan
	t.Cleanup(func() { textutil.CredentialRE, credentialScan = oldRE, oldScan })
	pat := regexp.QuoteMeta(memoryCredentialPlaceholder)
	re := regexp.MustCompile(pat)
	textutil.CredentialRE = func() *regexp.Regexp { return re }
	credentialScan = func() credentialScanner {
		return credentialScanner{
			lits:      [][]byte{[]byte(memoryCredentialPlaceholder)},
			atStart:   regexp.MustCompile(`\A(?:` + pat + `)`),
			afterByte: regexp.MustCompile(`\A(?s:.)(?:` + pat + `)`),
		}
	}
	return memoryCredentialPlaceholder
}

// Doctor and the commit gate must agree on findings, independently of vendor token shapes.
func TestDoctorAndCommitGateAgree(t *testing.T) {
	placeholder := useCredentialPlaceholder(t)
	cases := map[string]bool{placeholder: true, "ordinary prose!": false}
	isolatedConfig(t)
	for v, want := range cases {
		path := Path("progress.md")
		write(t, path, "- note "+v+"\n")
		doctorFlags := len(statBank().Credentials) > 0
		gateRefuses := checkStageContent(path, "progress.md") != nil
		if doctorFlags != want || gateRefuses != want {
			t.Errorf("%q: doctor flags %v, gate refuses %v, want both %v", v, doctorFlags, gateRefuses, want)
		}
	}
}

func TestCommitStagesGlobNamedFilesLiterally(t *testing.T) {
	store, _ := storeFixture(t)
	r := StoreRepo()
	if err := r.Ensure(); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(store, "why?.md"), "glob-named\n")
	write(t, filepath.Join(store, "whyx.md"), "sibling the glob would match\n")
	if _, committed, err := r.Commit([]string{"why?.md"}, "literal", ""); err != nil || !committed {
		t.Fatalf("Commit(why?.md) = %v, %v; a glob pathspec staged the sibling", committed, err)
	}
	if got := hosttest.In(t, store, "ls-tree", "--name-only", "HEAD"); got != "why?.md\n" {
		t.Fatalf("HEAD tree = %q, want only why?.md", got)
	}
}

func TestStoreGitIgnoresTheUsersGlobalExcludes(t *testing.T) {
	store, _ := storeFixture(t)
	r := StoreRepo()
	if err := r.Ensure(); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "git", "ignore"), "*.json\nreports/\n")
	write(t, filepath.Join(store, "receipts", "x.json"), "{}\n")
	write(t, filepath.Join(store, "reports", "a.md"), "report\n")
	st, err := r.Status()
	if err != nil || !slices.Contains(st, statusEntry{"??", "receipts/x.json"}) || !slices.Contains(st, statusEntry{"??", "reports/a.md"}) {
		t.Fatalf("a global ignore hid store files from Status: %+v, %v", st, err)
	}
	if _, committed, err := r.Commit([]string{"receipts/x.json", "reports/a.md"}, "receipts", ""); err != nil || !committed {
		t.Fatalf("a global ignore broke Commit: %v, %v", committed, err)
	}
}

// seedRecoveryFact commits r/fact.md and returns the store dir.
func seedRecoveryFact(t *testing.T) string {
	t.Helper()
	store, _ := storeFixture(t)
	if _, err := batchLocked("seed", []storeChange{{Rel: "r/fact.md", Data: []byte("preserve me\n")}}); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestCommitFailureLeavesFilesForNextSettle(t *testing.T) {
	store := seedRecoveryFact(t)
	lock := filepath.Join(store, ".git", "refs", "heads", "main.lock")
	write(t, lock, "")
	_, err := batchLocked("delete", []storeChange{{Rel: "r/fact.md", Delete: true}})
	if err == nil || errors.Is(err, errDivergent) {
		t.Fatalf("held ref lock must fail the deletion commit: %v", err)
	}
	if _, err := os.Stat(filepath.Join(store, "r", "fact.md")); !os.IsNotExist(err) {
		t.Fatalf("the applied deletion must stay on disk: %v", err)
	}
	if staged, err := StoreRepo().run("diff", "--cached", "--name-only"); err != nil || staged != "" {
		t.Fatalf("failed deletion left the index staged: %q, %v", staged, err)
	}
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	if _, err := batchLocked("next", []storeChange{{Rel: "r/next.md", Data: []byte("next\n")}}); err != nil {
		t.Fatalf("next batch could not commit the deletion: %v", err)
	}
	if out, err := StoreRepo().run("ls-tree", "--name-only", "HEAD", "--", "r/fact.md"); err != nil || out != "" {
		t.Fatalf("deletion still present at HEAD: %q, %v", out, err)
	}
	if status, err := StoreRepo().Status(); err != nil || len(status) != 0 {
		t.Fatalf("store is dirty: %+v, %v", status, err)
	}
}

func TestCommitChangesRollsBackOnWriteError(t *testing.T) {
	hosttest.SkipOnWindows(t, "sh scripts, symlinks or errno")
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory write permissions")
	}
	store := seedRecoveryFact(t)
	blocker := filepath.Join(store, "r", "blocked")
	if err := os.MkdirAll(blocker, 0o755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(blocker, "keep.md")
	write(t, keep, "keep\n")
	if err := os.Chmod(blocker, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocker, 0o700) })
	rec, err := batchLocked("two", []storeChange{
		{Rel: "r/fact.md", Data: []byte("changed\n")},
		{Rel: "r/blocked/new.md", Data: []byte("x\n")},
	})
	if err == nil {
		t.Fatal("writing into a read-only directory must fail")
	}
	if got := readStore(t, "r/fact.md"); got != "preserve me\n" {
		t.Fatalf("the earlier file was not rolled back: %q", got)
	}
	if rec.Applied != nil || mustRead(t, keep) != "keep\n" {
		t.Fatalf("rollback receipt or blocker changed: %+v", rec)
	}
}

// decide classifies a change against the live bytes: stale preimages diverge, equal bytes skip.
func TestChangeDecide(t *testing.T) {
	store, _ := storeFixture(t)
	live := filepath.Join(store, "r", "a.md")
	write(t, live, "v1\n")
	pre := textutil.SHA256Hex([]byte("v1\n"))
	c := storeChange{Rel: "r/a.md", Data: []byte("v2\n"), Planned: true, PreSHA256: pre}
	for _, tc := range []struct {
		live string
		c    storeChange
		want changeState
	}{
		{"v1\n", c, stApply},
		{"edited\n", c, stDiverge},
		{"v2\n", c, stSkip},
		{"edited\n", storeChange{Rel: "r/a.md", Data: []byte("v2\n")}, stApply},
	} {
		write(t, live, tc.live)
		if got, err := tc.c.decide(store); err != nil || got != tc.want {
			t.Fatalf("live %q: got %v, %v; want %v", tc.live, got, err, tc.want)
		}
	}
	if got, _ := (storeChange{Rel: "r/gone.md", Delete: true}).decide(store); got != stSkip {
		t.Fatalf("delete of an absent file = %v, want skip", got)
	}
}

func TestChangeWriteKeepsLateEdits(t *testing.T) {
	store, _ := storeFixture(t)
	live := filepath.Join(store, "r", "a.md")
	write(t, live, "first\n")
	snap, err := snapshotFile(live)
	if err != nil {
		t.Fatal(err)
	}
	c := storeChange{Rel: "r/a.md", Data: []byte("output\n")}
	write(t, live, "late\n")
	var rec commitReceipt
	if err := c.write(StoreRepo(), live, stApply, snap, &rec); err != nil {
		t.Fatal(err)
	}
	if got := readStore(t, "r/a.md"); got != "late\n" || !slices.Equal(rec.Divergent, []string{"r/a.md"}) {
		t.Fatalf("late edit overwritten: %q, %+v", got, rec)
	}
	write(t, live, "first\n")
	rec = commitReceipt{}
	if err := c.write(StoreRepo(), live, stSkip, snap, &rec); err != nil {
		t.Fatal(err)
	}
	if got := readStore(t, "r/a.md"); got != "first\n" || !slices.Equal(rec.Divergent, []string{"r/a.md"}) {
		t.Fatalf("uncaptured edit overwritten: %q, %+v", got, rec)
	}
}

func TestCommitKeepsPendingJournalFile(t *testing.T) {
	storeFixture(t)
	pending := filepath.Join(JournalDir(), "pending.json")
	write(t, pending, "pending bytes\n")
	rec, err := commitLocked(t, "batch", []storeChange{{Rel: "r/a.md", Data: []byte("a\n")}})
	if err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, pending); got != "pending bytes\n" {
		t.Fatalf("pending journal changed: %q", got)
	}
	if slices.Contains(rec.Preexisting, "pending.json") || slices.Contains(rec.Applied, "pending.json") {
		t.Fatalf("pending journal included in the batch: %+v", rec)
	}
}

func TestRollbackKeepsNativeEdit(t *testing.T) {
	store, _ := storeFixture(t)
	live := filepath.Join(store, "r", "a.md")
	write(t, live, "before\n")
	snap, err := snapshotFile(live)
	if err != nil {
		t.Fatal(err)
	}
	w := appliedWrite{change: storeChange{Rel: "r/a.md", Data: []byte("batch\n")}, snap: snap}
	write(t, live, "native edit\n")
	if err := w.restoreIfOurs(); err == nil || readStore(t, "r/a.md") != "native edit\n" {
		t.Fatalf("native edit overwritten or error lost: %v", err)
	}
	write(t, live, "batch\n")
	if err := w.restoreIfOurs(); err != nil || readStore(t, "r/a.md") != "before\n" {
		t.Fatalf("batch bytes not rolled back: %v", err)
	}
}

func TestCaptureNativeKeepsEditMissedByStatus(t *testing.T) {
	store := seedRecoveryFact(t)
	changes := []storeChange{{Rel: "r/fact.md", Data: []byte("batch\n")}}
	pre, _ := os.Lstat(filepath.Join(store, "r/fact.md"))
	write(t, filepath.Join(store, "r/fact.md"), "late native edit\n")
	var rec commitReceipt
	if _, _, err := captureNative(StoreRepo(), "batch", changes, nil, []fs.FileInfo{pre}, &rec); err != nil {
		t.Fatal(err)
	}
	if got, err := StoreRepo().run("show", "HEAD:r/fact.md"); err != nil || got != "late native edit" {
		t.Fatalf("native revision not captured: %q, %v", got, err)
	}
}

func TestCommitRejectsDuplicatePaths(t *testing.T) {
	store := seedRecoveryFact(t)
	_, err := batchLocked("duplicates", []storeChange{
		{Rel: "r/fact.md", Data: []byte("one\n")}, {Rel: "r/fact.md", Data: []byte("two\n")},
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate path") {
		t.Fatalf("duplicate paths accepted: %v", err)
	}
	if got := mustRead(t, filepath.Join(store, "r/fact.md")); got != "preserve me\n" {
		t.Fatalf("refused batch changed the file: %q", got)
	}
}
