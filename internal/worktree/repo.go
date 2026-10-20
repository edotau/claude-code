// Package worktree is the deterministic Go lifecycle for ~/worktrees/<repo>/<name>: identity,
// listing/classification, and safe create/clean. Shared by the worktree-create/-remove hooks and the
// `claude-code worktree` verb; later Appendix A phases add merge/resolve/status on top of it.
package worktree

import (
	"context"
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
	out, err := exec.CommandContext(context.Background(), "git", "-C", cwd, "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
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
	return resolveWorktree(context.Background(), repo, name)
}

func resolveWorktree(ctx context.Context, repo *Repo, name string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\") {
		return "", fmt.Errorf("worktree name %q is not usable", name)
	}
	dest := filepath.Join(repo.Root, name)
	if filepath.Dir(dest) != filepath.Clean(repo.Root) {
		return "", fmt.Errorf("worktree name %q escapes %s", name, repo.Root)
	}
	if err := rejectLiveCheckoutRoot(repo); err != nil {
		return "", err
	}
	info, err := os.Lstat(dest)
	if err != nil {
		return dest, nil // nothing there yet — the caller decides whether that's fine
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%s is a symlink, refusing to treat it as a worktree", dest)
	}
	owns, err := ownsWorktree(ctx, repo, dest)
	if err != nil {
		return "", err
	}
	if !owns {
		return "", fmt.Errorf("%s exists but is not a worktree of %s (basename collision)", dest, repo.Main)
	}
	return dest, nil
}

// rejectLiveCheckoutRoot refuses repo.Root when it canonically IS, or lives inside, repo.Main — the
// check ownsWorktree does for an EXISTING dest, run here unconditionally so a worktree root symlinked
// into the live checkout is rejected even before `create` would otherwise materialize a brand-new
// directory inside it. repo.Root (or its parent, WorktreeRoot()) is resolved via its longest EXISTING
// prefix — plain canonical() would silently skip every symlink when repo.Root's own basename doesn't
// exist yet, which is exactly the case on a first `create` for this repo.
func rejectLiveCheckoutRoot(repo *Repo) error {
	rootCanon, err := canonicalExistingPrefix(repo.Root)
	if err != nil {
		return err
	}
	mainCanon, err := canonical(repo.Main)
	if err != nil {
		return err
	}
	if rootCanon == mainCanon || strings.HasPrefix(rootCanon+string(filepath.Separator), mainCanon+string(filepath.Separator)) {
		return fmt.Errorf("worktree root %s is the live main checkout %s (or inside it), refusing to use it", repo.Root, repo.Main)
	}
	return nil
}

// canonicalExistingPrefix resolves symlinks in the longest prefix of p that actually exists on disk,
// then rejoins the remaining (not-yet-created) suffix unresolved — so a path whose leaf doesn't exist
// yet still gets every real symlink component (e.g. a symlinked worktree root) resolved, unlike
// canonical()/EvalSymlinks, which require the entire path to exist.
func canonicalExistingPrefix(p string) (string, error) {
	p = filepath.Clean(p)
	suffix := ""
	cur := p
	for {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(resolved, suffix), nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p, nil // reached the filesystem root without finding anything that exists
		}
		suffix = filepath.Join(filepath.Base(cur), suffix)
		cur = parent
	}
}

// ownsWorktree reports whether dest is itself a worktree sharing repo's common git dir — the only
// reliable way to tell "this is our worktree" from "something else already lives at this path". It
// rejects dest outright when it is the live main checkout or a subdirectory of it (e.g. a symlinked
// ~/worktrees/<repo> pointing straight at the main checkout would otherwise let a name like "internal"
// resolve to the live checkout's own subdirectory), and requires dest to be a worktree *root* — its own
// `--show-toplevel` must equal dest — so a subdirectory of some other checkout can't pass common-dir
// equality alone and be mistaken for a worktree.
func ownsWorktree(ctx context.Context, repo *Repo, dest string) (bool, error) {
	destCanon, err := canonical(dest)
	if err != nil {
		return false, err
	}
	mainCanon, err := canonical(repo.Main)
	if err != nil {
		return false, err
	}
	if destCanon == mainCanon || strings.HasPrefix(destCanon+string(filepath.Separator), mainCanon+string(filepath.Separator)) {
		return false, fmt.Errorf("%s is the live main checkout %s (or inside it), not a worktree", dest, repo.Main)
	}

	topOut, err := exec.CommandContext(ctx, "git", "-C", dest, "rev-parse", "--path-format=absolute", "--show-toplevel").Output()
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, nil // not a git repo at all is a collision, not a resolution error
	}
	top, err := canonical(strings.TrimSpace(string(topOut)))
	if err != nil {
		return false, err
	}
	if top != destCanon {
		return false, nil // dest is a subdirectory of some checkout's worktree root, not a worktree root itself
	}

	out, err := exec.CommandContext(ctx, "git", "-C", dest, "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, nil
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
