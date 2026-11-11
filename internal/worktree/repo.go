// Package worktree is the deterministic Go lifecycle for ~/worktrees/<repo>/<name>: identity,
// listing/classification, and safe create/clean. Shared by the worktree-create/-remove hooks and the
// `claude-code worktree` verb, with merge and agent-assisted resolve on top.
package worktree

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
)

// Repo is a git repository's canonical identity for worktree purposes: the main checkout (never a
// linked worktree) plus the conventional root holding every worktree created or discovered for it.
type Repo struct {
	Main string // canonical main checkout path
	Root string // Root(Main): ~/worktrees/<basename(Main)>
	// CommonDir is the absolute git common dir ResolveRepo saw; "" (a hand-built Repo) means look it up.
	CommonDir string
}

// worktreeRoot is the one worktree home shared with native Claude Code: ~/worktrees, under the
// canonical home so it compares equal to the resolved paths git reports (a symlinked $HOME would otherwise
// disown every registered worktree).
func worktreeRoot() string {
	home, _ := os.UserHomeDir()
	home = paths.Canon(home)
	return filepath.Join(home, "worktrees")
}

// Root is repo main's own worktree root: worktreeRoot()/<basename of the main checkout>.
func Root(main string) string {
	return filepath.Join(worktreeRoot(), filepath.Base(filepath.Clean(main)))
}

// gitCmd builds a git command with per-repo GIT_* environment variables scrubbed.
func gitCmd(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = gitEnv()
	return cmd
}

// ResolveRepo resolves cwd (a main checkout, a linked worktree, or any subdirectory of either) to its
// canonical git identity. --git-common-dir is absolute and identical from anywhere inside the repo, so
// this is the one call that makes worktree/subdir/main all agree without reading .git pointer files.
func ResolveRepo(cwd string) (*Repo, error) {
	ctx, cancel := gitTimeoutCtx()
	defer cancel()
	out, err := gitCmd(ctx, "-C", cwd, "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
	if err != nil {
		return nil, fmt.Errorf("%s is not inside a git repository", cwd)
	}
	commonDir := strings.TrimSpace(string(out))
	if filepath.Base(commonDir) != ".git" {
		return nil, fmt.Errorf("%s: bare or non-standard git layout is not supported", cwd)
	}
	main := paths.Canon(filepath.Dir(commonDir))
	return &Repo{Main: main, Root: Root(main), CommonDir: commonDir}, nil
}

// resolveWorktreeBounded validates name and returns the path a worktree called name occupies under
// repo.Root, rejecting traversal, a symlink at the destination, and a name already occupied by a
// directory that is not actually a worktree of this repo (a foreign basename collision).
func resolveWorktreeBounded(repo *Repo, name string) (string, error) {
	ctx, cancel := gitTimeoutCtx()
	defer cancel()
	return resolveWorktree(ctx, repo, name)
}

func resolveWorktree(ctx context.Context, repo *Repo, name string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	dest, err := checkWorktreeName(repo, name)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(dest)
	if err != nil {
		if os.IsNotExist(err) {
			return dest, nil // nothing there yet — the caller decides whether that's fine
		}
		return "", fmt.Errorf("inspect %s: %w", dest, err)
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

// checkWorktreeName validates name and returns its destination under repo.Root, rejecting a live checkout root.
func checkWorktreeName(repo *Repo, name string) (string, error) {
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
	return dest, nil
}

// rejectLiveCheckoutRoot refuses repo.Root when it canonically IS, or lives inside, repo.Main — the
// check ownsWorktree does for an EXISTING dest, run here unconditionally so a worktree root symlinked
// into the live checkout is rejected even before `create` would otherwise materialize a brand-new
// directory inside it. repo.Root (or its parent, worktreeRoot()) is resolved via its longest EXISTING
// prefix — plain paths.Canon() would silently skip every symlink when repo.Root's own basename doesn't
// exist yet, which is exactly the case on a first `create` for this repo.
func rejectLiveCheckoutRoot(repo *Repo) error {
	rootCanon := paths.CanonDeep(repo.Root)
	mainCanon := paths.Canon(repo.Main)
	if paths.Within(rootCanon, mainCanon) {
		return fmt.Errorf("worktree root %s is the live main checkout %s (or inside it), refusing to use it", repo.Root, repo.Main)
	}
	return nil
}

// ownsWorktree reports whether dest is itself a worktree sharing repo's common git dir — the only
// reliable way to tell "this is our worktree" from "something else already lives at this path". It
// rejects dest outright when it is the live main checkout or a subdirectory of it (e.g. a symlinked
// ~/worktrees/<repo> pointing straight at the main checkout would otherwise let a name like "internal"
// resolve to the live checkout's own subdirectory), and requires dest to be a worktree *root* — its own
// `--show-toplevel` must equal dest — so a subdirectory of some other checkout can't pass common-dir
// equality alone and be mistaken for a worktree.
func ownsWorktree(ctx context.Context, repo *Repo, dest string) (bool, error) {
	destCanon := paths.Canon(dest)
	mainCanon := paths.Canon(repo.Main)
	if paths.Within(destCanon, mainCanon) {
		return false, fmt.Errorf("%s is the live main checkout %s (or inside it), not a worktree", dest, repo.Main)
	}
	isRoot, err := isWorktreeRoot(ctx, dest, destCanon)
	if err != nil || !isRoot {
		return false, err
	}
	return sharesCommonDir(ctx, repo, dest)
}

// isWorktreeRoot reports whether dest's own `--show-toplevel` is destCanon; a non-git dest is a collision, not an error.
func isWorktreeRoot(ctx context.Context, dest, destCanon string) (bool, error) {
	topOut, err := gitCmd(ctx, "-C", dest, "rev-parse", "--path-format=absolute", "--show-toplevel").Output()
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, nil // not a git repo at all is a collision, not a resolution error
	}
	top := paths.Canon(strings.TrimSpace(string(topOut)))
	return top == destCanon, nil // otherwise dest is a subdirectory of some checkout's worktree root
}

// sharesCommonDir reports whether dest's git common dir is repo's own.
func sharesCommonDir(ctx context.Context, repo *Repo, dest string) (bool, error) {
	out, err := gitCmd(ctx, "-C", dest, "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, nil
	}
	common := paths.Canon(strings.TrimSpace(string(out)))
	wantCommon := paths.Canon(filepath.Join(repo.Main, ".git"))
	return common == wantCommon, nil
}
