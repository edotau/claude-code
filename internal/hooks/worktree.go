package hooks

import (
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/worktree"
)

// WorktreeCreate replaces Claude Code's `.claude/worktrees/<name>` with <root>/<repo>/<name>; stdout is the path.
// Unlike other hooks it fails CLOSED: a non-zero exit is the only way to report a worktree that was not made.
func WorktreeCreate(r io.Reader, stdout, stderr io.Writer) int {
	in := ParseInput(r)
	// The live payload carries only `name`; the documented worktree_path/branch are honored if they ever appear.
	name := in.nested("name")
	if name == "" {
		name = filepath.Base(filepath.Clean(in.nested("worktree_path")))
	}
	repo, err := worktree.ResolveRepo(in.CWD)
	if err != nil {
		fmt.Fprintf(stderr, "worktree-create: %v\n", err)
		return 1
	}
	dest, err := worktree.Create(repo, name, worktree.CreateOptions{
		Branch: in.nested("branch"), Session: in.SessionID, Log: stderr,
	})
	if err != nil {
		fmt.Fprintf(stderr, "worktree-create: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, dest)
	return ExitProceed
}

// WorktreeRemove removes a hook-created worktree; no --force, so a dirty tree stays and the removal reports failure.
func WorktreeRemove(r io.Reader, stderr io.Writer) int {
	in := ParseInput(r)
	path := in.nested("worktree_path")
	if path == "" {
		fmt.Fprintln(stderr, "worktree-remove: payload has no worktree_path")
		return 1
	}
	if reason, err := worktree.InUseBy(path); err != nil {
		fmt.Fprintf(stderr, "worktree-remove: in-use check failed: %v\n", err)
		return 1
	} else if reason != "" {
		fmt.Fprintf(stderr, "worktree-remove: %s\n", reason)
		return 1
	}
	ctx, cancel := hookContext("worktree-remove")
	defer cancel()
	main := paths.MainCheckout(path)
	branch, _ := exec.CommandContext(ctx, "git", "-C", path, "branch", "--show-current").Output()
	head, _ := exec.CommandContext(ctx, "git", "-C", path, "rev-parse", "HEAD").Output()
	if out, err := exec.CommandContext(ctx, "git", "-C", main, "worktree", "remove", path).CombinedOutput(); err != nil {
		fmt.Fprintf(stderr, "worktree-remove: %v: %s\n", err, strings.TrimSpace(string(out)))
		return 1
	}
	branchDeleted := false
	if b := strings.TrimSpace(string(branch)); b != "" {
		branchDeleted = exec.CommandContext(ctx, "git", "-C", main, "branch", "-d", b).Run() == nil
	}
	if err := worktree.CloseLedger(main, filepath.Base(filepath.Clean(path)), worktree.LedgerClose{
		Via: "remove", Head: strings.TrimSpace(string(head)), BranchDeleted: branchDeleted,
	}); err != nil {
		fmt.Fprintf(stderr, "worktree-remove: warning: ledger: %v\n", err)
	}
	return ExitProceed
}
