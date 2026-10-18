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
	dest, err := worktree.Create(repo, name, in.nested("branch"))
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
	main := paths.MainCheckout(path)
	branch, _ := exec.Command("git", "-C", path, "branch", "--show-current").Output()
	if out, err := exec.Command("git", "-C", main, "worktree", "remove", path).CombinedOutput(); err != nil {
		fmt.Fprintf(stderr, "worktree-remove: %v: %s\n", err, strings.TrimSpace(string(out)))
		return 1
	}
	if b := strings.TrimSpace(string(branch)); b != "" {
		_ = exec.Command("git", "-C", main, "branch", "-d", b).Run() // -d keeps a branch holding unmerged commits
	}
	return ExitProceed
}
