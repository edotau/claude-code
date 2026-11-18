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

// watchWrites snapshots workdir (top: its git toplevel, "" asks git); its func lists changed and re-touched files.
func watchWrites(ctx context.Context, workdir, top string) func() (changed, touched *[]string) {
	unobserved := func() (*[]string, *[]string) { return nil, nil }
	root := top
	if root == "" {
		gctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writesTimeout)
		root = paths.GitTop(gctx, workdir)
		cancel()
	}
	if root == "" {
		return unobserved
	}
	before, err := snapTree(ctx, root, nil)
	if err != nil {
		return unobserved
	}
	return func() (*[]string, *[]string) {
		after, err := snapTree(ctx, root, before)
		if err != nil {
			return nil, nil
		}
		changed, touched := []string{}, []string{}
		for p, sig := range after {
			was, dirty := before[p]
			if dirty && was != sig {
				touched = append(touched, p)
			}
			if !dirty || was != sig {
				changed = append(changed, p)
			}
		}
		if len(touched) == 0 {
			return capPaths(changed), nil
		}
		return capPaths(changed), capPaths(touched)
	}
}

func capPaths(list []string) *[]string {
	slices.Sort(list)
	if len(list) > writesCap {
		list = append(list[:writesCap], WroteTruncated)
	}
	return &list
}

// sharesTree reports whether another dispatch in rec's git tree overlapped rec's run, which ended at end.
func sharesTree(rec Record, end time.Time) bool {
	dir := paths.DispatchDir()
	names, err := recordNames(dir)
	if rec.Tree == "" || err != nil {
		return false
	}
	for _, n := range names {
		o, err := readRecord(filepath.Join(dir, n))
		if err != nil || o.ID == rec.ID || o.Tree != rec.Tree {
			continue
		}
		oEnd := o.Time.Add(time.Duration(o.DurationMs) * time.Millisecond)
		if o.Alive() {
			oEnd = end
		}
		if o.Time.Before(end) && oEnd.After(rec.Time) {
			return true
		}
	}
	return false
}
