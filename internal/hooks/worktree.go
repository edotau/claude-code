package hooks

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
)

// WorktreeRoot is the one worktree home shared with Orca: <root>/<repo>/<name>, the repo's main-checkout basename.
func WorktreeRoot() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "worktrees")
}

// WorktreeCreate replaces Claude Code's `.claude/worktrees/<name>` with <root>/<repo>/<name>; stdout is the path.
// Unlike other hooks it fails CLOSED: a non-zero exit is the only way to report a worktree that was not made.
func WorktreeCreate(r io.Reader, stdout, stderr io.Writer) int {
	in := ParseInput(r)
	// The live payload carries only `name`; the documented worktree_path/branch are honored if they ever appear.
	name := in.nested("name")
	if name == "" {
		name = filepath.Base(filepath.Clean(in.nested("worktree_path")))
	}
	dest, err := createWorktree(in.CWD, name, in.nested("branch"))
	if err != nil {
		fmt.Fprintf(stderr, "worktree-create: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, dest)
	return ExitProceed
}

func createWorktree(cwd, name, branch string) (string, error) {
	top := memory.GitToplevel(cwd)
	if top == "" {
		return "", fmt.Errorf("%s is not inside a git repository", cwd)
	}
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, filepath.Separator) {
		return "", fmt.Errorf("payload has no usable worktree name (%q)", name)
	}
	if branch == "" {
		branch = "worktree-" + name
	}
	main := paths.MainCheckout(top)
	dest := filepath.Join(WorktreeRoot(), filepath.Base(filepath.Clean(main)), name)
	if paths.SamePath(paths.MainCheckout(dest), main) {
		return dest, nil // `--worktree <name>` again resumes the existing checkout
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	args := []string{"-C", main, "worktree", "add", dest}
	if exec.Command("git", "-C", main, "show-ref", "--verify", "--quiet", "refs/heads/"+branch).Run() == nil {
		args = append(args, branch) // reuse an existing branch rather than failing on -b
	} else {
		args = append(args, "-b", branch, "HEAD")
	}
	if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
		return "", fmt.Errorf("git worktree add: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return dest, nil
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
