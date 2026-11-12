// Package worktree is the lifecycle for ~/worktrees/<repo>/<name>: identity, list, create/clean, merge, agent resolve.
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

// gitFatalExit is the exit status git uses for "fatal:" errors such as running outside a repository.
const gitFatalExit = 128

// Repo is a repository's canonical identity: the main checkout (never a linked worktree) and its worktree root.
type Repo struct {
	Main string // canonical main checkout path
	Root string // Root(Main): ~/worktrees/<basename(Main)>
	// CommonDir is the absolute git common dir ResolveRepo saw; "" (a hand-built Repo) means look it up.
	CommonDir string
}

// worktreeRoot is ~/worktrees under the canonical home, so a symlinked $HOME still matches git's resolved paths.
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

// ResolveRepo resolves cwd (main, linked worktree or a subdirectory) to its identity via the absolute --git-common-dir.
func ResolveRepo(cwd string) (*Repo, error) {
	ctx, cancel := gitTimeoutCtx()
	defer cancel()
	out, err := gitCmd(ctx, "-C", cwd, "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
	if code, isExit := asExitErr(err); isExit && code == gitFatalExit && ctx.Err() == nil {
		return nil, fmt.Errorf("%s is not inside a git repository", cwd)
	} else if err != nil {
		return nil, fmt.Errorf("git rev-parse in %s: %w", cwd, err)
	}
	return repoFromCommonDir(cwd, strings.TrimSpace(string(out)))
}

// repoFromCommonDir builds the Repo for an absolute git common dir, rejecting a bare or non-standard layout.
func repoFromCommonDir(cwd, commonDir string) (*Repo, error) {
	if filepath.Base(commonDir) != ".git" {
		return nil, fmt.Errorf("%s: bare or non-standard git layout is not supported", cwd)
	}
	if _, err := os.UserHomeDir(); err != nil {
		return nil, fmt.Errorf("%s: cannot place worktrees without a home directory (set HOME): %w", cwd, err)
	}
	main := paths.Canon(filepath.Dir(commonDir))
	return &Repo{Main: main, Root: Root(main), CommonDir: commonDir}, nil
}

// resolveWorktreeBounded returns where name lives under repo.Root, rejecting traversal, symlinks and collisions.
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

// rejectLiveCheckoutRoot refuses a repo.Root that is, or sits inside, repo.Main, resolving its longest EXISTING prefix.
func rejectLiveCheckoutRoot(repo *Repo) error {
	rootCanon := paths.CanonDeep(repo.Root) // Canon would skip every symlink while Root's basename does not exist yet
	mainCanon := paths.Canon(repo.Main)
	if paths.Within(rootCanon, mainCanon) {
		return fmt.Errorf("worktree root %s is the live main checkout %s (or inside it), refusing to use it", repo.Root, repo.Main)
	}
	return nil
}

// ownsWorktree reports whether dest is a worktree root sharing repo's common dir, never the live checkout or inside it.
func ownsWorktree(ctx context.Context, repo *Repo, dest string) (bool, error) {
	destCanon := paths.Canon(dest) // a ~/worktrees/<repo> symlinked to main would let name "internal" resolve inside it
	mainCanon := paths.Canon(repo.Main)
	if paths.Within(destCanon, mainCanon) {
		return false, fmt.Errorf("%s is the live main checkout %s (or inside it), not a worktree", dest, repo.Main)
	}
	top, ok, err := revParseCanon(ctx, dest, "--show-toplevel")
	if err != nil || !ok || top != destCanon {
		return false, err // not a git repo is a collision, and a subdirectory of some checkout is not a worktree root
	}
	common, ok, err := revParseCanon(ctx, dest, "--git-common-dir")
	return ok && common == paths.Canon(filepath.Join(repo.Main, ".git")), err
}

// revParseCanon runs `git rev-parse --path-format=absolute flag` in dir; ok is false when git itself fails there.
func revParseCanon(ctx context.Context, dir, flag string) (path string, ok bool, err error) {
	out, err := gitCmd(ctx, "-C", dir, "rev-parse", "--path-format=absolute", flag).Output()
	if err != nil {
		return "", false, ctx.Err()
	}
	return paths.Canon(strings.TrimSpace(string(out))), true, nil
}
