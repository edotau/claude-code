// Package worktree is the deterministic Go lifecycle for ~/worktrees/<repo>/<name>: identity,
// listing/classification, and safe create/clean. Shared by the worktree-create/-remove hooks and the
// `claude-code worktree` verb; later Appendix A phases add merge/resolve/status on top of it.
package worktree

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Repo is a git repository's canonical identity for worktree purposes: the main checkout (never a
// linked worktree) plus the conventional root holding every worktree created or discovered for it.
type Repo struct {
	Main string // canonical main checkout path
	Root string // Root(Main): ~/worktrees/<basename(Main)>
}

// WorktreeRoot is the one worktree home shared with Orca and native Claude Code: ~/worktrees, under the
// canonical home so it compares equal to the resolved paths git reports (a symlinked $HOME would otherwise
// disown every registered worktree).
func WorktreeRoot() string {
	home, _ := os.UserHomeDir()
	home, _ = canonical(home)
	return filepath.Join(home, "worktrees")
}

// Root is repo main's own worktree root: WorktreeRoot()/<basename of the main checkout>.
func Root(main string) string {
	return filepath.Join(WorktreeRoot(), filepath.Base(filepath.Clean(main)))
}

// ResolveRepo resolves cwd (a main checkout, a linked worktree, or any subdirectory of either) to its
// canonical git identity. --git-common-dir is absolute and identical from anywhere inside the repo, so
// this is the one call that makes worktree/subdir/main all agree without reading .git pointer files.
func ResolveRepo(cwd string) (*Repo, error) {
	out, err := exec.Command("git", "-C", cwd, "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
	if err != nil {
		return nil, fmt.Errorf("%s is not inside a git repository", cwd)
	}
	commonDir := strings.TrimSpace(string(out))
	if filepath.Base(commonDir) != ".git" {
		return nil, fmt.Errorf("%s: bare or non-standard git layout is not supported", cwd)
	}
	main, err := canonical(filepath.Dir(commonDir))
	if err != nil {
		return nil, err
	}
	return &Repo{Main: main, Root: Root(main)}, nil
}

// ResolveWorktree validates name and returns the path a worktree called name occupies under
// repo.Root, rejecting traversal, a symlink at the destination, and a name already occupied by a
// directory that is not actually a worktree of this repo (a foreign basename collision).
func ResolveWorktree(repo *Repo, name string) (string, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\") {
		return "", fmt.Errorf("worktree name %q is not usable", name)
	}
	dest := filepath.Join(repo.Root, name)
	if filepath.Dir(dest) != filepath.Clean(repo.Root) {
		return "", fmt.Errorf("worktree name %q escapes %s", name, repo.Root)
	}
	info, err := os.Lstat(dest)
	if err != nil {
		return dest, nil // nothing there yet — the caller decides whether that's fine
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%s is a symlink, refusing to treat it as a worktree", dest)
	}
	owns, err := ownsWorktree(repo, dest)
	if err != nil {
		return "", err
	}
	if !owns {
		return "", fmt.Errorf("%s exists but is not a worktree of %s (basename collision)", dest, repo.Main)
	}
	return dest, nil
}

// ownsWorktree reports whether dest is itself a worktree sharing repo's common git dir — the only
// reliable way to tell "this is our worktree" from "something else already lives at this path".
func ownsWorktree(repo *Repo, dest string) (bool, error) {
	out, err := exec.Command("git", "-C", dest, "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
	if err != nil {
		return false, nil // not a git repo at all is a collision, not a resolution error
	}
	common, err := canonical(strings.TrimSpace(string(out)))
	if err != nil {
		return false, err
	}
	wantCommon, err := canonical(filepath.Join(repo.Main, ".git"))
	if err != nil {
		return false, err
	}
	return common == wantCommon, nil
}

// canonical cleans and resolves symlinks so two spellings of the same directory compare equal.
func canonical(p string) (string, error) {
	p = filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r, nil
	}
	return p, nil // unresolvable (e.g. already-deleted) path still compares consistently cleaned
}
