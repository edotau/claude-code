// Write record of a dispatch: the files its repository changed between the start and the end of its run.

package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
)

// writesTimeout bounds each git call around a run, which never gate it: a slow repo costs this much, not more.
const writesTimeout = 3 * time.Second

// writesCap is the most paths a record keeps; WroteTruncated is then its last entry and the list proves nothing.
const writesCap = 500

// WroteTruncated ends a Record.Wrote list cut at writesCap; no real path looks like it (paths are absolute).
const WroteTruncated = "(truncated)"

// treeSnap is the size and write time of every file git reports changed (absolute path), "absent" when gone.
type treeSnap map[string]string

// snapTree is the signature of every changed path under root and of every path in also.
func snapTree(ctx context.Context, root string, also treeSnap) (treeSnap, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writesTimeout)
	defer cancel()
	out, err := proc.RunGitRaw(ctx, root, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return nil, err
	}
	snap := treeSnap{}
	for _, e := range proc.StatusEntries(out) {
		for _, rel := range []string{e.Path, e.From} {
			if rel != "" {
				snap.add(filepath.Join(root, filepath.FromSlash(rel)))
			}
		}
	}
	for p := range also {
		snap.add(p)
	}
	return snap, nil
}

func (s treeSnap) add(path string) {
	sig := "absent"
	if fi, err := os.Lstat(path); err == nil {
		sig = fmt.Sprintf("%d:%d", fi.Size(), fi.ModTime().UnixNano())
	}
	s[path] = sig
}

// watchWrites snapshots workdir's repository; the func it returns lists the files changed since, nil if unobserved.
func watchWrites(ctx context.Context, workdir string) func() *[]string {
	gctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writesTimeout)
	root := paths.GitTop(gctx, workdir)
	cancel()
	if root == "" {
		return func() *[]string { return nil }
	}
	before, err := snapTree(ctx, root, nil)
	if err != nil {
		return func() *[]string { return nil }
	}
	return func() *[]string {
		after, err := snapTree(ctx, root, before)
		if err != nil {
			return nil
		}
		changed := []string{}
		for p, sig := range after {
			if was, ok := before[p]; !ok || was != sig {
				changed = append(changed, p)
			}
		}
		slices.Sort(changed)
		if len(changed) > writesCap {
			changed = append(changed[:writesCap], WroteTruncated)
		}
		return &changed
	}
}
