package worktree

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Create makes (or resumes) the worktree called name under repo.Root, on branch — defaulting to
// "worktree-"+name to match the native hook's naming — reusing an existing branch rather than
// failing on -b. Shared by the worktree-create hook and `claude-code worktree create`.
func Create(repo *Repo, name, branch string) (string, error) {
	dest, err := ResolveWorktree(repo, name)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(dest); err == nil {
		return dest, nil // `--worktree <name>` again resumes the existing checkout; ResolveWorktree already verified it's ours
	}
	if branch == "" {
		branch = "worktree-" + name
	}
	if exec.Command("git", "check-ref-format", "--branch", branch).Run() != nil {
		return "", fmt.Errorf("%q is not a valid branch name; pass --branch <name>", branch)
	}
	if err := os.MkdirAll(repo.Root, 0o755); err != nil {
		return "", err
	}
	args := []string{"-C", repo.Main, "worktree", "add", dest}
	if branchExists(repo.Main, branch) {
		args = append(args, branch) // reuse an existing branch rather than failing on -b
	} else {
		args = append(args, "-b", branch, "HEAD")
	}
	if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
		return "", fmt.Errorf("git worktree add: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return dest, nil
}

func branchExists(main, branch string) bool {
	return exec.Command("git", "-C", main, "show-ref", "--verify", "--quiet", "refs/heads/"+branch).Run() == nil
}

// CleanResult reports what Clean did, or would do under dryRun, to one worktree.
type CleanResult struct {
	Path          string
	Branch        string
	Removed       bool
	BranchDeleted bool
	Reason        string // populated when Removed is false: why cleanup was refused
}

// Clean removes name's worktree only after a live recheck confirms it is unlocked, clean, merged and
// owned by repo (ResolveWorktree's collision check); it never forces removal, and keeps the branch
// unless a safe `branch -d` succeeds. dryRun runs the same recheck and reports the verdict only.
func Clean(repo *Repo, name string, dryRun bool) (CleanResult, error) {
	dest, err := ResolveWorktree(repo, name)
	if err != nil {
		return CleanResult{}, err
	}
	if _, err := os.Stat(dest); err != nil {
		return CleanResult{}, fmt.Errorf("worktree %q not found at %s", name, dest)
	}
	branch, _ := currentWorktreeBranch(dest)
	result := CleanResult{Path: dest, Branch: branch}

	locked, err := isLocked(dest)
	if err != nil {
		result.Reason = "lock check failed: " + err.Error()
		return result, nil
	}
	if locked {
		result.Reason = "worktree is locked"
		return result, nil
	}

	entry := Entry{Path: dest, Head: headOf(dest)}
	target, targetErr := currentBranch(repo.Main)
	Inspect(repo, &entry, target, targetErr, time.Now())
	switch {
	case len(entry.Problems) > 0:
		result.Reason = "inspection failed: " + strings.Join(entry.Problems, "; ")
		return result, nil
	case entry.Dirty:
		result.Reason = "worktree has uncommitted changes"
		return result, nil
	case !entry.Merged:
		result.Reason = "worktree HEAD is not merged into " + target
		return result, nil
	}

	if dryRun {
		result.Removed = true // preview: every recheck above passed, so this one is eligible
		return result, nil
	}
	if out, err := exec.Command("git", "-C", repo.Main, "worktree", "remove", dest).CombinedOutput(); err != nil {
		result.Reason = fmt.Sprintf("git worktree remove: %v: %s", err, strings.TrimSpace(string(out)))
		return result, nil
	}
	result.Removed = true
	if branch != "" && exec.Command("git", "-C", repo.Main, "branch", "-d", branch).Run() == nil {
		result.BranchDeleted = true // -d refuses an unmerged branch; failure there just keeps it
	}
	return result, nil
}

// CleanAll previews (or, with apply, executes) Clean across every entry List finds that actually
// exists on disk and isn't already orphaned; auto-prune and deletion of unregistered directories are
// deferred to a later phase, so an orphan is only ever reported, never removed, by this call.
func CleanAll(repo *Repo, apply bool) ([]CleanResult, error) {
	entries, err := List(repo)
	if err != nil {
		return nil, err
	}
	var results []CleanResult
	for _, e := range entries {
		if !e.Exists || e.Orphaned {
			continue
		}
		r, err := Clean(repo, filepath.Base(e.Path), !apply)
		if err != nil {
			return results, err
		}
		results = append(results, r)
	}
	return results, nil
}

func currentWorktreeBranch(dest string) (string, error) {
	out, err := exec.Command("git", "-C", dest, "branch", "--show-current").Output()
	return strings.TrimSpace(string(out)), err
}

func headOf(dest string) string {
	out, err := exec.Command("git", "-C", dest, "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// isLocked stats the linked worktree's private "locked" marker directly, so Clean needs no full
// `worktree list` round trip just to recheck one entry's lock state.
func isLocked(dest string) (bool, error) {
	out, err := exec.Command("git", "-C", dest, "rev-parse", "--path-format=absolute", "--git-dir").Output()
	if err != nil {
		return false, err
	}
	_, statErr := os.Stat(filepath.Join(strings.TrimSpace(string(out)), "locked"))
	return statErr == nil, nil
}
