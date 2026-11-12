package worktree

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/memory"
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
	dest, err := resolveWorktreeBounded(repo, name)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(dest); err == nil {
		if err := resumeLedger(repo, name, dest, opt.Session); err != nil {
			fmt.Fprintf(log, "worktree-create: warning: ledger: %v\n", err)
		}
		return dest, nil // `--worktree <name>` again resumes the checkout; resolveWorktreeBounded verified it's ours
	}
	branch, err := createBranchName(name, opt.Branch)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(repo.Root, 0o755); err != nil {
		return "", err
	}
	args, baseRef := worktreeAddArgs(repo.Main, dest, branch, log)
	ctx, cancel := gitTimeoutCtx()
	defer cancel()
	base, baseErr := gitCmd(ctx, "-C", repo.Main, "rev-parse", "--verify", baseRef+"^{commit}").Output()
	if out, err := gitCmd(ctx, args...).CombinedOutput(); err != nil {
		return "", fmt.Errorf("git worktree add: %v: %s", err, strings.TrimSpace(string(out)))
	}
	seedWorktree(repo.Main, dest, log)
	if baseErr != nil {
		fmt.Fprintf(log, "worktree-create: warning: ledger: resolve base: %v\n", baseErr)
	} else if err := openLedger(repo.Main, name, ledgerInfo{
		Path: dest, Branch: branch, Base: strings.TrimSpace(string(base)), BaseRef: baseRef, Session: opt.Session,
	}); err != nil {
		fmt.Fprintf(log, "worktree-create: warning: ledger: %v\n", err)
	}
	return dest, nil
}

// createBranchName is the branch a new worktree name gets: want, else worktree-<name>; it must be a valid ref name.
func createBranchName(name, want string) (string, error) {
	branch := want
	if branch == "" {
		branch = "worktree-" + name
	}
	ctx, cancel := gitTimeoutCtx()
	defer cancel()
	if gitCmd(ctx, "check-ref-format", "--branch", branch).Run() != nil {
		return "", fmt.Errorf("%q is not a valid branch name; pass --branch <name>", branch)
	}
	return branch, nil
}

// worktreeAddArgs builds the `git worktree add` arguments for branch and the ref the worktree starts from.
func worktreeAddArgs(main, dest, branch string, log io.Writer) (args []string, baseRef string) {
	args = []string{"-C", main, "worktree", "add", dest}
	if branchExists(main, branch) {
		return append(args, branch), branch // reuse an existing branch rather than failing on -b
	}
	baseRef = createBase(main, log)
	return append(args, "--no-track", "-b", branch, baseRef), baseRef // no upstream: branch -d checks merged into HEAD
}

func resumeLedger(repo *Repo, name, dest, session string) error {
	path := ledgerPath(memory.Dir(repo.Main), name)
	if data, err := os.ReadFile(path); err == nil {
		if ledgerField(string(data), "Status") != "closed" {
			return nil
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	info, err := reopenInfo(repo, dest, session)
	if err != nil {
		return err
	}
	return openLedger(repo.Main, name, info)
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
		ctx, cancel := gitTimeoutCtx()
		defer cancel()
		out, err := gitCmd(ctx, "-C", main, "symbolic-ref", "refs/remotes/origin/HEAD").Output()
		ref := strings.TrimSpace(string(out))
		if err == nil && gitCmd(ctx, "-C", main, "rev-parse", "--verify", ref+"^{commit}").Run() == nil {
			return ref
		}
	}
	return "HEAD"
}

func branchExists(main, branch string) bool {
	ctx, cancel := gitTimeoutCtx()
	defer cancel()
	return gitCmd(ctx, "-C", main, "show-ref", "--verify", "--quiet", "refs/heads/"+branch).Run() == nil
}

// CleanResult reports what Clean did, or would do under dryRun, to one worktree.
type CleanResult struct {
	Path          string
	Branch        string
	Removed       bool
	BranchDeleted bool
	Reason        string // populated when Removed is false: why cleanup was refused
}

// Clean removes name's worktree only after a live recheck (unlocked, clean, merged, owned) under the lease; no force.
func Clean(repo *Repo, name string, dryRun bool) (CleanResult, error) {
	return cleanWithLog(repo, name, dryRun, os.Stderr)
}

// cleanWithLog is Clean with ledger warnings written to log (nil discards them).
func cleanWithLog(repo *Repo, name string, dryRun bool, log io.Writer) (CleanResult, error) {
	var result CleanResult
	err := WithWorktreeLease(repo, name, func() error {
		dest, err := resolveWorktreeBounded(repo, name)
		if err != nil {
			return err
		}
		result, err = cleanAtWith(repo, name, dest, dryRun, log, LedgerClose{Via: "clean"})
		return err
	})
	return result, err
}

// cleanAtWith rechecks and removes a dest validated under the lease, closing its ledger (Head defaults to HEAD).
func cleanAtWith(repo *Repo, name, dest string, dryRun bool, log io.Writer, closing LedgerClose) (CleanResult, error) {
	if log == nil {
		log = io.Discard
	}
	result, err := beginClean(name, dest, log)
	if err != nil {
		return CleanResult{}, err
	}

	gitDir, reason := cleanBusyReason(dest, nil)
	if reason != "" {
		result.Reason = reason
		return result, nil
	}
	entry, reason := cleanInspectReason(repo, dest, gitDir)
	if reason != "" {
		result.Reason = reason
		return result, nil
	}

	if dryRun {
		result.Removed = true // preview: every recheck above passed, so this one is eligible
		return result, nil
	}
	return removeMerged(repo, name, dest, entry, result, log, closing), nil
}

// beginClean checks dest exists and is no symlink, then seeds a CleanResult with its branch (log warns if unknown).
func beginClean(name, dest string, log io.Writer) (CleanResult, error) {
	if _, err := os.Stat(dest); err != nil {
		return CleanResult{}, fmt.Errorf("worktree %q not found at %s", name, dest)
	}
	if info, err := os.Lstat(dest); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return CleanResult{}, fmt.Errorf("%s is a symlink, refusing to treat it as a worktree", dest)
	}
	branch, branchErr := currentWorktreeBranch(dest)
	if branchErr != nil {
		fmt.Fprintf(log, "worktree-clean: warning: branch not detected, it will not be deleted: %v\n", branchErr)
	}
	return CleanResult{Path: dest, Branch: branch}, nil
}

// inUseReason is the refusal reason for a worktree some process is using ("" = free).
func inUseReason(dest string, in func(string) ([]Proc, error)) string {
	reason, err := inUseBy(dest, in)
	if err != nil {
		return "in-use check failed: " + err.Error()
	}
	return reason
}

// lockReason is the refusal reason for a failed or positive lock check ("" = unlocked).
func lockReason(locked bool, err error) string {
	if err != nil {
		return "lock check failed: " + err.Error()
	}
	if locked {
		return "worktree is locked"
	}
	return ""
}

// cleanBusyReason runs the in-use (lister in, nil = live), lock and operation checks in order; returns the git dir.
func cleanBusyReason(dest string, in func(string) ([]Proc, error)) (gitDir, reason string) {
	if reason := inUseReason(dest, in); reason != "" {
		return "", reason
	}
	ctx, cancel := gitTimeoutCtx()
	defer cancel()
	gitDir, err := gitDirOf(ctx, dest)
	if err != nil {
		return "", "lock check failed: " + err.Error()
	}
	locked, err := isLockedAt(gitDir)
	if reason := lockReason(locked, err); reason != "" {
		return "", reason
	}
	if op, found, err := inProgressOp(ctx, dest, gitDir); err != nil {
		return "", "operation check failed: " + err.Error()
	} else if found {
		return "", op + " in progress"
	}
	return gitDir, ""
}

// cleanInspectReason refuses a worktree whose inspection failed, is dirty, or is not merged into the target.
func cleanInspectReason(repo *Repo, dest, gitDir string) (entry Entry, reason string) {
	entry = Entry{Path: dest, Head: headOf(dest), gitDir: gitDir}
	target, targetErr := currentBranch(repo.Main)
	inspect(repo, &entry, target, targetErr, time.Now())
	switch {
	case len(entry.Problems) > 0:
		return entry, "inspection failed: " + strings.Join(entry.Problems, "; ")
	case entry.Dirty:
		return entry, "worktree has uncommitted changes"
	case !entry.Merged:
		return entry, "worktree HEAD is not merged into " + target
	}
	return entry, ""
}

// removeMerged removes a worktree that passed every recheck, deletes its merged branch and closes the ledger once.
func removeMerged(repo *Repo, name, dest string, entry Entry, result CleanResult, log io.Writer, closing LedgerClose) CleanResult {
	ctx, cancel := gitTimeoutCtx()
	defer cancel()
	if out, err := gitCmd(ctx, "-C", repo.Main, "worktree", "remove", dest).CombinedOutput(); err != nil {
		result.Reason = fmt.Sprintf("git worktree remove: %v: %s", err, strings.TrimSpace(string(out)))
		return result
	}
	result.Removed = true
	if result.Branch != "" && gitCmd(ctx, "-C", repo.Main, "branch", "-d", result.Branch).Run() == nil {
		result.BranchDeleted = true // -d refuses an unmerged branch; failure there just keeps it
	}
	closing.BranchDeleted = result.BranchDeleted
	if closing.Head == "" {
		closing.Head = entry.Head
	}
	if err := CloseLedger(repo.Main, name, closing); err != nil {
		fmt.Fprintf(log, "worktree-clean: warning: ledger: %v\n", err)
	}
	return result
}

// CleanAll previews (or, with apply, runs) Clean for every existing, non-orphaned entry; orphans are only reported.
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
		if filepath.Dir(e.Path) != filepath.Clean(repo.Root) {
			results = append(results, CleanResult{Path: e.Path, Reason: "nested below " + repo.Root + ", not a <root>/<name> worktree; not cleaned"})
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

// gitTimeoutCtx bounds one lifecycle git call so a hung git cannot hold the worktree lease forever.
func gitTimeoutCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), probeTimeout)
}

func currentWorktreeBranch(dest string) (string, error) {
	ctx, cancel := gitTimeoutCtx()
	defer cancel()
	out, err := gitCmd(ctx, "-C", dest, "branch", "--show-current").Output()
	return strings.TrimSpace(string(out)), err
}

func headOf(dest string) string {
	ctx, cancel := gitTimeoutCtx()
	defer cancel()
	head, _ := headOfContext(ctx, dest) // an error leaves head empty
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

// isLockedAt stats the "locked" marker directly; only a confirmed IsNotExist is unlocked, any other error propagates.
func isLockedAt(gitDir string) (bool, error) { return markerExists(gitDir, "locked") }

// DiscardSummary is what CleanForce is about to throw away, reported before anything is removed.
type DiscardSummary struct {
	Path, Branch, Target string
	Dirty, Unmerged      int
}

// CleanForce removes name even when dirty or unmerged and force-deletes its branch; still refuses in-use, locked, main.
func CleanForce(repo *Repo, name string, dryRun bool, report func(DiscardSummary)) (CleanResult, error) {
	var result CleanResult
	err := WithWorktreeLease(repo, name, func() error {
		r, cleanErr := cleanForceInLease(repo, name, dryRun, report, os.Stderr)
		result = r
		return cleanErr
	})
	return result, err
}

func cleanForceInLease(repo *Repo, name string, dryRun bool, report func(DiscardSummary), log io.Writer) (CleanResult, error) {
	dest, err := resolveWorktreeBounded(repo, name) // refuses traversal, symlinks, foreign dirs, a root inside Main
	if err != nil {
		return CleanResult{}, err
	}
	result, err := beginClean(name, dest, log)
	if err != nil {
		return CleanResult{}, err
	}

	if reason := forceBusyReason(repo, dest); reason != "" {
		result.Reason = reason
		return result, nil
	}
	summary, reason := discardSummary(repo, dest, result.Branch)
	if reason != "" {
		result.Reason = reason
		return result, nil
	}
	if report != nil {
		report(summary)
	}

	if dryRun {
		result.Removed = true // preview: the remaining refusals passed, so this one would be discarded
		return result, nil
	}
	return removeForced(repo, name, dest, result, log), nil
}

// forceBusyReason refuses the main checkout, an in-use worktree and a locked one, in that order.
func forceBusyReason(repo *Repo, dest string) string {
	if isMainCheckout(repo, dest) {
		return "refusing to remove the main checkout"
	}
	if reason := inUseReason(dest, nil); reason != "" {
		return reason
	}
	ctx, cancel := gitTimeoutCtx()
	defer cancel()
	gitDir, err := gitDirOf(ctx, dest)
	if err != nil {
		return lockReason(false, err)
	}
	return lockReason(isLockedAt(gitDir))
}

// discardSummary counts what a forced clean would throw away; a non-empty reason refuses it.
func discardSummary(repo *Repo, dest, branch string) (DiscardSummary, string) {
	ctx, cancel := gitTimeoutCtx()
	defer cancel()
	target, err := requireBranch(ctx, repo.Main)
	if err != nil || target == "" {
		return DiscardSummary{}, "cannot determine target branch: main checkout is detached or unreadable"
	}
	status, err := gitCmd(ctx, "-C", dest, "status", "--porcelain").Output()
	if err != nil {
		return DiscardSummary{}, "status check failed: " + err.Error()
	}
	count, err := gitCmd(ctx, "-C", dest, "rev-list", "--count", target+"..HEAD").Output()
	if err != nil {
		return DiscardSummary{}, "unmerged count failed: " + err.Error()
	}
	unmerged, err := strconv.Atoi(strings.TrimSpace(string(count)))
	if err != nil {
		return DiscardSummary{}, "unmerged count unreadable: " + err.Error()
	}
	summary := DiscardSummary{Path: dest, Branch: branch, Target: target, Unmerged: unmerged}
	if s := strings.TrimRight(string(status), "\n"); s != "" {
		summary.Dirty = len(strings.Split(s, "\n"))
	}
	return summary, ""
}

// removeForced force-removes the worktree, force-deletes its branch and closes the ledger.
func removeForced(repo *Repo, name, dest string, result CleanResult, log io.Writer) CleanResult {
	ctx, cancel := gitTimeoutCtx()
	defer cancel()
	head := headOf(dest)
	if out, err := gitCmd(ctx, "-C", repo.Main, "worktree", "remove", "--force", dest).CombinedOutput(); err != nil {
		result.Reason = fmt.Sprintf("git worktree remove --force: %v: %s", err, strings.TrimSpace(string(out)))
		return result
	}
	result.Removed = true
	if branch := result.Branch; branch != "" {
		if out, err := gitCmd(ctx, "-C", repo.Main, "branch", "-D", branch).CombinedOutput(); err != nil {
			result.Reason = fmt.Sprintf("git branch -D %s: %v: %s", branch, err, strings.TrimSpace(string(out)))
		} else {
			result.BranchDeleted = true
		}
	}
	if err := CloseLedger(repo.Main, name, LedgerClose{Via: "clean --force", Head: head, BranchDeleted: result.BranchDeleted}); err != nil {
		fmt.Fprintf(log, "worktree-clean: warning: ledger: %v\n", err)
	}
	return result
}

// isMainCheckout reports whether dest canonically is repo.Main; an unresolvable path counts as main.
func isMainCheckout(repo *Repo, dest string) bool {
	d, err1 := filepath.EvalSymlinks(dest)
	m, err2 := filepath.EvalSymlinks(repo.Main)
	return err1 != nil || err2 != nil || d == m
}
