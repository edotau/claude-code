package memory

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
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
func assertFreshRepoEmpty(t *testing.T, r Repo) {
	t.Helper()
	if head, _ := r.Head(); head != "" {
		t.Fatalf("fresh repo has HEAD %q", head)
	}
	if log, err := r.Log(5); err != nil || log != nil {
		t.Fatalf("Log on empty repo = %v, %v", log, err)
	}
	if remotes, err := r.remotes(); err != nil || len(remotes) != 0 {
		t.Fatalf("fresh repo remotes() = %v, %v; want none", remotes, err)
	}
}

// assertPrePushRefuses runs the hook and requires exit status 1.
func assertPrePushRefuses(t *testing.T, hookPath string) {
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
	assertDecoyUntouched(t, decoy)
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
	log, err := r.Log(10)
	if err != nil || len(log) != 2 || !strings.HasSuffix(log[0], " delete") {
		t.Fatalf("Log = %v, %v", log, err)
	}
	if head, _ := r.Head(); head == "" {
		t.Fatal("Head empty after commits")
	}
	assertDecoyUntouched(t, decoy)
}

func TestCommitRejectsBadPathsAndSymlinks(t *testing.T) {
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
	r := Repo{Dir: filepath.Join(t.TempDir(), "repo")}
	if err := r.Ensure(); err != nil {
		t.Fatal(err)
	}
	if head, err := r.Head(); err != nil || head != "" {
		t.Fatalf("unborn Head = %q, %v, want \"\", nil", head, err)
	}
	if _, err := (Repo{Dir: filepath.Join(t.TempDir(), "missing")}).Head(); err == nil {
		t.Fatal("Head on a non-repo = nil error, want the git failure")
	}
}

// credentialPositives are real secret shapes, assembled from parts so no secret-shaped literal sits in the source.
func credentialPositives() map[string]string {
	return map[string]string{
		"sk-proj":      "sk-" + "proj-" + strings.Repeat("a1B2", 10),
		"sk-ant":       "sk-" + "ant-api03-" + strings.Repeat("x9Y_", 6),
		"sk classic":   "sk-" + strings.Repeat("a1B2c", 4),
		"dapi":         "dapi" + strings.Repeat("0f", 16),
		"jwt":          "ey" + "J" + strings.Repeat("hbGciOi", 3) + "." + strings.Repeat("eyJzdWIi", 3),
		"nvapi":        "nvapi-" + strings.Repeat("Ab1-", 7),
		"ghp":          "gh" + "p_" + strings.Repeat("A1b2", 9),
		"github_pat":   "github" + "_pat_" + strings.Repeat("11AB", 6) + "_" + strings.Repeat("cd34", 15),
		"akia":         "AK" + "IA" + strings.Repeat("Q7", 8),
		"rsa private":  "-----BEGIN " + "RSA PRIVATE KEY-----",
		"bare private": "-----BEGIN " + "PRIVATE KEY-----",
	}
}

// credentialNegatives were the loose pattern's false positives: kebab prose, public material, too-short fragments.
func credentialNegatives() map[string]string {
	return map[string]string{
		"kebab task":  "task-abcdefghij12",
		"kebab mask":  "mask-0123456789ab",
		"certificate": "-----BEGIN " + "CERTIFICATE-----",
		"pgp public":  "-----BEGIN " + "PGP PUBLIC KEY BLOCK-----",
		"short dapi":  "dapi" + "0123456789ab",
		"short sk":    "sk-" + "short12345",
		"akia inside": "SLOV" + "AKIA" + strings.Repeat("BC", 8),
	}
}

func TestCredentialPatternMatchesSecretShapes(t *testing.T) {
	for name, v := range credentialPositives() {
		if !credentialPattern.MatchString("note: " + v + " here") {
			t.Errorf("%s: secret shape not matched", name)
		}
	}
}

func TestCredentialPatternSkipsFalsePositives(t *testing.T) {
	for name, v := range credentialNegatives() {
		if credentialPattern.MatchString("note: " + v + " here") {
			t.Errorf("%s: %q flagged as a credential", name, v)
		}
	}
}

// TestDoctorAndCommitGateAgree drives doctor's statBank and the commit gate's checkStageContent on one file each.
func TestDoctorAndCommitGateAgree(t *testing.T) {
	cases := map[string]bool{}
	for _, v := range credentialPositives() {
		cases[v] = true
	}
	for _, v := range credentialNegatives() {
		cases[v] = false
	}
	for v, want := range cases {
		dir := t.TempDir()
		path := filepath.Join(dir, "progress.md")
		write(t, path, "- note "+v+"\n")
		doctorFlags := len(statBank(dir).Credentials) > 0
		gateRefuses := checkStageContent(path, "progress.md") != nil
		if doctorFlags != want || gateRefuses != want {
			t.Errorf("%q: doctor flags %v, gate refuses %v, want both %v", v, doctorFlags, gateRefuses, want)
		}
	}
}

// Regression: the commit gate missed shapes the config-history scanner holds back (Tavily, Google, Groq, AWS STS, Slack).
func TestCommitGateRefusesHistoryScannerShapes(t *testing.T) {
	cases := map[string]string{
		"tavily":  "tvly-" + strings.Repeat("Fk", 12),
		"google":  "AI" + "za" + strings.Repeat("Fk1", 11) + "Fk",
		"groq":    "gsk_" + strings.Repeat("Fk", 15),
		"aws sts": "AS" + "IA" + strings.Repeat("FK", 8),
		"slack":   "xoxb-" + "1234567890-FAKEFAKEFAKE",
	}
	for name, v := range cases {
		path := filepath.Join(t.TempDir(), "progress.md")
		write(t, path, "- fact "+v+"\n")
		if checkStageContent(path, "progress.md") == nil {
			t.Errorf("%s: commit gate accepted a credential-shaped fact", name)
		}
	}
}
