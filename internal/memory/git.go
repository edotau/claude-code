package memory

// Bounded, sanitized git for the nested local memory repo at StoreDir() (plan C.3). Explicit --git-dir and
// --work-tree are what keep an inherited GIT_DIR or a missing .git from retargeting the OUTER ~/.claude checkout;
// never call bare `git -C StoreDir()` here. Shares internal/proc's sanitized env with internal/history.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/proc"
)

// Repo is the nested memory repository rooted at Dir (its work tree); .git lives directly inside it.
type Repo struct{ Dir string }

// StoreRepo is the live memory store's repo.
func StoreRepo() Repo { return Repo{Dir: StoreDir()} }

// gitTimeout bounds one git call; a var so tests need not wait it out.
var gitTimeout = 30 * time.Second

// stageSecretRE matches credential-shaped values; \b-anchored like history.secretRE so `sk-` never eats kebab prose.
var stageSecretRE = regexp.MustCompile(`\bdapi[0-9a-f]{32}|\beyJ[A-Za-z0-9_-]{16,}\.[A-Za-z0-9_-]{16,}|\bsk-[A-Za-z0-9_-]{24,}|\bnvapi-[A-Za-z0-9_-]{24,}|\bgh[pousr]_[A-Za-z0-9]{28,}|\bAKIA[A-Z0-9]{16}\b|-----BEGIN [A-Z ]*PRIVATE KEY`)

// prePushHook refuses every push: a guard against accidents, not an access-control boundary.
const prePushHook = "#!/bin/sh\n# memory is local-only (plan C.3): refuse every push; a guard against accidents, not access control.\n" +
	"echo 'memory: this repository is local-only; push refused' >&2\nexit 1\n"

// storeGitignore keeps runtime beacons beside memory files out of history; locks live under state now.
const storeGitignore = "# harness runtime beacons beside memory files — never history\n.archive*\naccess.log\n*.tmp-*\n"

// Exists reports a real nested repo: <Dir>/.git is a directory (a gitdir: pointer file is a foreign worktree).
func (r Repo) Exists() bool {
	fi, err := os.Lstat(filepath.Join(r.Dir, ".git"))
	return err == nil && fi.IsDir()
}

// Ensure initializes the repo when absent and verifies its root, distinctness from the outer checkout and lack of
// remotes BEFORE writing the pre-push guard and .gitignore, so a redirected store never receives a byte.
func (r Repo) Ensure() error {
	if err := os.MkdirAll(r.Dir, 0o700); err != nil {
		return err
	}
	if fi, err := os.Lstat(r.Dir); err != nil {
		return err
	} else if !fi.IsDir() { // a symlinked root Lstats as a link: it could redirect the store into any repo
		return fmt.Errorf("memory repo: %s is not a plain directory (symlink?)", r.Dir)
	}
	gitDir := filepath.Join(r.Dir, ".git")
	if !r.Exists() {
		if _, err := os.Lstat(gitDir); err == nil {
			return fmt.Errorf("memory repo: %s exists but is not a directory (foreign worktree?)", gitDir)
		}
		if _, err := r.run("init", "--quiet", "--initial-branch=main"); err != nil {
			return err
		}
	}
	if err := r.checkRoot(gitDir); err != nil {
		return err
	}
	if remotes, err := r.Remotes(); err != nil {
		return err
	} else if len(remotes) > 0 {
		return fmt.Errorf("memory repo is local-only but has remotes: %s", strings.Join(remotes, ", "))
	}
	if err := writeIfAbsent(filepath.Join(gitDir, "hooks", "pre-push"), prePushHook, 0o755); err != nil {
		return err
	}
	return writeIfAbsent(filepath.Join(r.Dir, ".gitignore"), storeGitignore, 0o644)
}

// checkRoot proves plain discovery from inside Dir lands on gitDir (not an empty .git or a parent's), that no commondir
// file shares another repo's refs/objects, and that the enclosing checkout, if any, does not share it.
func (r Repo) checkRoot(gitDir string) error {
	found, err := gitOut(r.Dir, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return err
	}
	want := resolvePath(gitDir)
	if resolvePath(found) != want {
		return fmt.Errorf("memory repo: git in %s resolves to %q, not %q", r.Dir, found, gitDir)
	}
	common, err := gitOut(r.Dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	if resolvePath(common) != want {
		return fmt.Errorf("memory repo: %s shares git metadata with %q (commondir)", gitDir, common)
	}
	outer, err := gitOut(filepath.Dir(r.Dir), "rev-parse", "--absolute-git-dir")
	if err == nil && resolvePath(outer) == want {
		return fmt.Errorf("memory repo: %s is the outer checkout's git dir", gitDir)
	}
	return nil
}

// writeIfAbsent creates path with body unless it already exists (a user's own edit is never overwritten).
func writeIfAbsent(path, body string, mode os.FileMode) error {
	if _, err := os.Lstat(path); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(body), mode)
}

// Remotes lists configured remote names; the store must have none.
func (r Repo) Remotes() ([]string, error) {
	out, err := r.run("remote")
	if err != nil {
		return nil, err
	}
	return splitLines(out), nil
}

// StatusEntry is one porcelain v1 row: the XY code and the path (a rename reports its NEW path).
type StatusEntry struct{ Code, Path string }

// Status lists dirty paths (untracked files individually), limited to pathspecs when given.
func (r Repo) Status(pathspecs ...string) ([]StatusEntry, error) {
	args := []string{"status", "--porcelain=v1", "-z", "--untracked-files=all"}
	if len(pathspecs) > 0 {
		args = append(append(args, "--"), pathspecs...)
	}
	out, err := r.run(args...)
	if err != nil {
		return nil, err
	}
	var entries []StatusEntry
	fields := strings.Split(out, "\x00")
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if len(f) < 4 {
			continue
		}
		code := f[:2]
		entries = append(entries, StatusEntry{Code: code, Path: f[3:]})
		if code[0] == 'R' || code[0] == 'C' {
			i++ // the original path follows as its own NUL field
		}
	}
	return entries, nil
}

// Head is the short sha of HEAD, "" when the repo has no commits yet.
func (r Repo) Head() (string, error) {
	out, err := r.run("rev-parse", "--verify", "--quiet", "--short", "HEAD")
	if err != nil {
		return "", nil
	}
	return out, nil
}

// Log lists the n newest commits as "<sha> <date> <subject>"; nil when there are no commits.
func (r Repo) Log(n int) ([]string, error) {
	if head, err := r.Head(); err != nil || head == "" {
		return nil, err
	}
	out, err := r.run("log", "-n", strconv.Itoa(n), "--format=%h %ad %s", "--date=short")
	if err != nil {
		return nil, err
	}
	return splitLines(out), nil
}

// Commit stages exactly rels (store-relative; an absent tracked rel stages its deletion) and commits when the
// index changed. Rejections name the path only; file bytes never reach the error.
func (r Repo) Commit(rels []string, subject, body string) (sha string, committed bool, err error) {
	stage, err := r.stageable(rels)
	if err != nil {
		return "", false, err
	}
	if len(stage) > 0 {
		if _, err := r.run(append([]string{"add", "-A", "--"}, stage...)...); err != nil {
			return "", false, err
		}
	}
	if err := r.checkStaged(rels); err != nil {
		return "", false, err
	}
	if _, err := r.run("diff", "--cached", "--quiet"); err == nil {
		return "", false, nil
	} else if exitCode(err) != 1 {
		return "", false, err
	}
	args := []string{"commit", "--quiet", "-m", subject}
	if body != "" {
		args = append(args, "-m", body)
	}
	if _, err := r.run(args...); err != nil {
		return "", false, err
	}
	sha, err = r.Head()
	return sha, true, err
}

// stageable validates rels and drops the absent-and-untracked ones (`add` rejects a pathspec matching nothing).
func (r Repo) stageable(rels []string) ([]string, error) {
	var present, absent []string
	for _, rel := range rels {
		if err := r.validateRel(rel); err != nil {
			return nil, err
		}
		abs := filepath.Join(r.Dir, rel)
		if _, err := os.Lstat(abs); err != nil {
			absent = append(absent, rel)
			continue
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			return nil, err
		}
		if stageSecretRE.Match(data) {
			return nil, fmt.Errorf("credential-shaped content: %s", rel)
		}
		present = append(present, rel)
	}
	if len(absent) == 0 {
		return present, nil
	}
	out, err := r.run(append([]string{"ls-files", "-z", "--"}, absent...)...)
	if err != nil {
		return nil, err
	}
	for tracked := range strings.SplitSeq(out, "\x00") {
		if tracked != "" {
			present = append(present, tracked)
		}
	}
	return present, nil
}

// checkStaged refuses an index holding anything beyond rels: a stray `git add` (or a pathspec glob) skipped validation.
func (r Repo) checkStaged(rels []string) error {
	out, err := r.run("diff", "--cached", "--name-only", "--no-renames", "-z")
	if err != nil {
		return err
	}
	want := make(map[string]bool, len(rels))
	for _, rel := range rels {
		want[filepath.ToSlash(rel)] = true
	}
	for p := range strings.SplitSeq(out, "\x00") {
		if p != "" && !want[p] {
			return fmt.Errorf("memory repo: unexpected staged change: %s", p)
		}
	}
	return nil
}

// validateRel rejects anything that is not a plain store-relative path, through plain directories only (no symlinked
// ancestor can redirect a write outside the store), to a regular (or absent) file.
func (r Repo) validateRel(rel string) error {
	bad := fmt.Errorf("memory repo: invalid path %q", rel)
	if rel == "" || rel == "." || rel == ".." || filepath.IsAbs(rel) || filepath.Clean(rel) != rel {
		return bad
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if parts[0] == ".git" {
		return bad
	}
	abs := r.Dir
	for i, p := range parts {
		if p == ".." {
			return bad
		}
		abs = filepath.Join(abs, p)
		fi, err := os.Lstat(abs)
		if err != nil {
			continue // absent: the write creates plain directories
		}
		if leaf := i == len(parts)-1; leaf && !fi.Mode().IsRegular() {
			return fmt.Errorf("memory repo: not a regular file: %s", rel)
		} else if !leaf && !fi.IsDir() {
			return fmt.Errorf("memory repo: not a plain directory: %s", strings.Join(parts[:i+1], "/"))
		}
	}
	return nil
}

// run executes git against this repo with explicit dirs, a throwaway identity, no signing and NO hooks (core.hooksPath
// at /dev/null); the installed pre-push guard still fires for manual git, which reads the repo's own hooks dir.
func (r Repo) run(args ...string) (string, error) {
	gitDir := filepath.Join(r.Dir, ".git")
	full := []string{"-C", r.Dir, "--git-dir=" + gitDir, "--work-tree=" + r.Dir,
		"-c", "user.name=claude-code", "-c", "user.email=claude-code@localhost",
		"-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false",
		"-c", "core.hooksPath=" + os.DevNull}
	return runGit(append(full, args...))
}

// gitOut runs `git -C dir args` with the same sanitized env and timeout; for identity probes of OTHER repos only.
func gitOut(dir string, args ...string) (string, error) {
	return runGit(append([]string{"-C", dir}, args...))
}

// runGit is the one exec seam, via internal/proc: env minus every GIT_* (an inherited GIT_DIR
// overrides --git-dir), bounded, trimming ONLY trailing newlines (porcelain carries meaning in column 0).
// full already carries its own -C/-c flags, so dir stays "" here to avoid a duplicate -C.
func runGit(full []string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	return proc.RunGit(ctx, "", full...)
}

// exitCode is the git process exit status wrapped in err, -1 when err is not an exit error.
func exitCode(err error) int {
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		return ee.ExitCode()
	}
	return -1
}

// splitLines splits trimmed output into non-empty lines.
func splitLines(out string) []string {
	var lines []string
	for l := range strings.SplitSeq(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}
