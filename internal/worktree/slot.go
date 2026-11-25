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
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(top)))[:8]
	for i := range slotCount {
		base := filepath.Join(root, fmt.Sprintf("slot-%s-%d", key, i))
		f, err := os.OpenFile(base+".lock", os.O_CREATE|os.O_RDWR, 0o600)
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
	rel, err := filepath.Rel(SlotRoot(), s.Dir)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("refusing to reset %s: not under the slot root %s", s.Dir, SlotRoot())
	}
	if _, err := os.Stat(filepath.Join(s.Dir, ".git")); err == nil {
		if _, err = proc.RunGit(ctx, s.Dir, "checkout", "--force", "--detach", sha); err == nil {
			if _, err = proc.RunGit(ctx, s.Dir, "clean", "-ffdxq"); err == nil {
				return nil
			}
		}
	}
	RemoveTree(ctx, s.top, s.Dir)
	_, err = proc.RunGit(ctx, s.top, "worktree", "add", "--detach", s.Dir, sha)
	return err
}

// RemoveTree deletes a linked worktree directory and prunes its record.
func RemoveTree(ctx context.Context, top, dir string) {
	_, _ = proc.RunGit(ctx, top, "worktree", "remove", "--force", dir)
	_ = os.RemoveAll(dir)
	_, _ = proc.RunGit(ctx, top, "worktree", "prune")
}
