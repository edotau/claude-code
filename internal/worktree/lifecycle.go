package worktree

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// CreateOptions controls branch selection and lifecycle diagnostics; nil Log discards diagnostics.
type CreateOptions struct {
	Branch, Session string
	Log             io.Writer
}

// Create makes or resumes name under repo.Root, reusing an existing branch when available.
func Create(repo *Repo, name string, opt CreateOptions) (string, error) {
	log := opt.Log
	if log == nil {
		log = io.Discard
	}
	dest, err := ResolveWorktree(repo, name)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(dest); err == nil {
		return dest, nil // `--worktree <name>` again resumes the existing checkout; ResolveWorktree already verified it's ours
	}
	branch := opt.Branch
	if branch == "" {
		branch = "worktree-" + name
	}
	if gitCmd(context.Background(), "check-ref-format", "--branch", branch).Run() != nil {
		return "", fmt.Errorf("%q is not a valid branch name; pass --branch <name>", branch)
	}
	if err := os.MkdirAll(repo.Root, 0o755); err != nil {
		return "", err
	}
	args := []string{"-C", repo.Main, "worktree", "add", dest}
	if branchExists(repo.Main, branch) {
		args = append(args, branch) // reuse an existing branch rather than failing on -b
	} else {
		args = append(args, "-b", branch, createBase(repo.Main, log))
	}
	if out, err := gitCmd(context.Background(), args...).CombinedOutput(); err != nil {
		return "", fmt.Errorf("git worktree add: %v: %s", err, strings.TrimSpace(string(out)))
	}
	seedWorktree(repo.Main, dest, log)
	return dest, nil
}

func createBase(main string, log io.Writer) string {
	baseRef := "fresh"
	for _, path := range []string{
		filepath.Join(paths.ConfigDir(), "settings.json"),
		filepath.Join(main, ".claude", "settings.json"),
		filepath.Join(main, ".claude", "settings.local.json"),
	} {
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		var settings struct {
			Worktree struct {
				BaseRef string `json:"baseRef"`
			} `json:"worktree"`
		}
		if err == nil {
			err = json.Unmarshal(data, &settings)
		}
		if err != nil {
			fmt.Fprintf(log, "worktree-create: warning: read %s: %v\n", path, err)
			continue
		}
		switch settings.Worktree.BaseRef {
		case "fresh", "head":
			baseRef = settings.Worktree.BaseRef
		}
	}
	if baseRef == "fresh" {
		out, err := gitCmd(context.Background(), "-C", main, "symbolic-ref", "refs/remotes/origin/HEAD").Output()
		ref := strings.TrimSpace(string(out))
		if err == nil && gitCmd(context.Background(), "-C", main, "rev-parse", "--verify", ref+"^{commit}").Run() == nil {
			return ref
		}
	}
	return "HEAD"
}

func branchExists(main, branch string) bool {
	return gitCmd(context.Background(), "-C", main, "show-ref", "--verify", "--quiet", "refs/heads/"+branch).Run() == nil
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
// unless a safe `branch -d` succeeds. dryRun runs the same recheck and reports the verdict only. The
// whole recheck-and-remove runs under the per-worktree lease (WithWorktreeLease), so it can never
// interleave with an active resolve or merge on the same worktree.
func Clean(repo *Repo, name string, dryRun bool) (CleanResult, error) {
	var result CleanResult
	err := WithWorktreeLease(repo, name, func() error {
		r, cleanErr := CleanInLease(repo, name, dryRun)
		result = r
		return cleanErr
	})
	return result, err
}

// CleanInLease is Clean's recheck-and-remove body, exported for a caller that already holds
// WithWorktreeLease for repo+name (the CLI's merge lifecycle, which must not re-acquire the same
// non-reentrant lease after a resolve or promote already took it). Standalone callers use Clean.
func CleanInLease(repo *Repo, name string, dryRun bool) (CleanResult, error) {
	dest, err := ResolveWorktree(repo, name)
	if err != nil {
		return CleanResult{}, err
	}
	if _, err := os.Stat(dest); err != nil {
		return CleanResult{}, fmt.Errorf("worktree %q not found at %s", name, dest)
	}
	branch, _ := currentWorktreeBranch(dest)
	result := CleanResult{Path: dest, Branch: branch}

	if reason, err := InUseBy(dest); err != nil {
		result.Reason = "in-use check failed: " + err.Error()
		return result, nil
	} else if reason != "" {
		result.Reason = reason
		return result, nil
	}

	locked, err := isLocked(dest)
	if err != nil {
		result.Reason = "lock check failed: " + err.Error()
		return result, nil
	}
	if locked {
		result.Reason = "worktree is locked"
		return result, nil
	}

	if op, found, err := inProgressOp(context.Background(), dest); err != nil {
		result.Reason = "operation check failed: " + err.Error()
		return result, nil
	} else if found {
		result.Reason = op + " in progress"
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
	if out, err := gitCmd(context.Background(), "-C", repo.Main, "worktree", "remove", dest).CombinedOutput(); err != nil {
		result.Reason = fmt.Sprintf("git worktree remove: %v: %s", err, strings.TrimSpace(string(out)))
		return result, nil
	}
	result.Removed = true
	if branch != "" && gitCmd(context.Background(), "-C", repo.Main, "branch", "-d", branch).Run() == nil {
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
	out, err := gitCmd(context.Background(), "-C", dest, "branch", "--show-current").Output()
	return strings.TrimSpace(string(out)), err
}

func headOf(dest string) string {
	head, _ := headOfContext(context.Background(), dest) // error -> "": same contract as the old standalone impl
	return head
}

func headOfContext(ctx context.Context, dest string) (string, error) {
	out, err := gitCmd(ctx, "-C", dest, "rev-parse", "HEAD").Output()
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("resolve HEAD in %s: %w", dest, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// isLocked stats the linked worktree's private "locked" marker directly, so Clean needs no full
// `worktree list` round trip just to recheck one entry's lock state. Only a confirmed os.IsNotExist
// counts as unlocked; any other stat error is an unknown state and must propagate.
func isLocked(dest string) (bool, error) {
	out, err := gitCmd(context.Background(), "-C", dest, "rev-parse", "--path-format=absolute", "--git-dir").Output()
	if err != nil {
		return false, err
	}
	_, statErr := os.Stat(filepath.Join(strings.TrimSpace(string(out)), "locked"))
	switch {
	case statErr == nil:
		return true, nil
	case os.IsNotExist(statErr):
		return false, nil
	default:
		return false, statErr
	}
}
