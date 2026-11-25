package worktree

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
)

const slotCount = 4

// Slot is a private checkout of one commit, held by a lock file until Release.
type Slot struct {
	Dir     string
	top     string
	release func()
}

// SlotRoot is the directory holding every slot and fallback tree.
func SlotRoot() string { return filepath.Join(os.TempDir(), "claude-code-verify") }

// AcquireSlot locks a stable per-repo slot at sha; with every slot busy it uses a private tree named by own.
func AcquireSlot(ctx context.Context, top, sha, own string) (*Slot, error) {
	root := SlotRoot()
	if err := ensureRoot(root); err != nil {
		return nil, err
	}
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(top)))[:8]
	for i := range slotCount {
		base := filepath.Join(root, fmt.Sprintf("slot-%s-%d", key, i))
		if err := refuseSymlink(base); err != nil {
			return nil, err
		}
		f, err := paths.OpenNoFollow(base+".lock", os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, err
		}
		if !paths.TryLock(f) {
			_ = f.Close()
			continue
		}
		s := &Slot{Dir: base, top: top, release: func() { _ = f.Close() }}
		if err := s.Reset(ctx, sha); err != nil {
			s.Release()
			return nil, err
		}
		return s, nil
	}
	dir := filepath.Join(root, "tree-"+own)
	RemoveTree(ctx, top, dir) // a reused pid's leftover is ours
	if _, err := proc.RunGit(ctx, top, "worktree", "add", "--detach", dir, sha); err != nil {
		return nil, err
	}
	return &Slot{Dir: dir, top: top, release: func() { RemoveTree(context.Background(), top, dir) }}, nil
}

// Release hands the slot back; a fallback tree is removed.
func (s *Slot) Release() { s.release() }

// Reset moves the slot's worktree to sha and drops untracked and ignored files, recreating it when missing or broken.
func (s *Slot) Reset(ctx context.Context, sha string) error {
	if err := checkUnderRoot(s.Dir); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(s.Dir, ".git")); err == nil {
		busy, err := InUseBy(s.Dir)
		if err != nil {
			return err
		}
		if busy != "" {
			return fmt.Errorf("refusing to reset %s: %s", s.Dir, busy)
		}
		if _, err = proc.RunGit(ctx, s.Dir, "checkout", "--force", "--detach", sha); err == nil {
			if _, err = proc.RunGit(ctx, s.Dir, "clean", "-ffdxq"); err == nil {
				return nil
			}
		}
	}
	RemoveTree(ctx, s.top, s.Dir)
	_, err := proc.RunGit(ctx, s.top, "worktree", "add", "--detach", s.Dir, sha)
	return err
}

// ensureRoot creates the slot root and refuses one that is a symlink, foreign-owned or group/other-writable.
func ensureRoot(root string) error {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	fi, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("slot root %s is not a real directory (symlink?)", root)
	}
	if uid, ok := paths.OwnerUID(fi); ok {
		if uid != os.Getuid() {
			return fmt.Errorf("slot root %s is owned by uid %d, not %d", root, uid, os.Getuid())
		}
		if fi.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("slot root %s is group/other-writable (%v)", root, fi.Mode().Perm())
		}
	}
	return nil
}

// refuseSymlink errors when path exists as a symlink.
func refuseSymlink(path string) error {
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing symlink in slot root: %s", path)
	}
	return nil
}

// checkUnderRoot requires dir, symlinks resolved, to be a strict descendant of the resolved slot root.
func checkUnderRoot(dir string) error {
	root, err := filepath.EvalSymlinks(SlotRoot())
	if err != nil {
		return fmt.Errorf("refusing to reset %s: slot root: %w", dir, err)
	}
	if err := refuseSymlink(dir); err != nil {
		return err
	}
	real, err := filepath.EvalSymlinks(dir)
	if os.IsNotExist(err) { // a missing slot is recreated: resolve its parent only
		var parent string
		if parent, err = filepath.EvalSymlinks(filepath.Dir(dir)); err == nil {
			real = filepath.Join(parent, filepath.Base(dir))
		}
	}
	if err != nil {
		return fmt.Errorf("refusing to reset %s: %w", dir, err)
	}
	rel, err := filepath.Rel(root, real)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("refusing to reset %s: not under the slot root %s", dir, root)
	}
	return nil
}

// RemoveTree deletes a linked worktree directory and prunes its record.
func RemoveTree(ctx context.Context, top, dir string) {
	_, _ = proc.RunGit(ctx, top, "worktree", "remove", "--force", dir)
	_ = os.RemoveAll(dir)
	_, _ = proc.RunGit(ctx, top, "worktree", "prune")
}
