// Write record of a dispatch: the files its repository changed between the start and the end of its run.

package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
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

// treeHolds reports whether workdir ("" is the process cwd) is inside top, both symlink-resolved as git reports a top.
func treeHolds(top, workdir string) bool {
	wd, err := filepath.Abs(workdir)
	if err != nil {
		return false
	}
	real, err := filepath.EvalSymlinks(wd)
	if err != nil {
		return false
	}
	realTop, err := filepath.EvalSymlinks(top)
	return err == nil && paths.Within(real, realTop)
}

// watchWrites observes the workdir's repo, else the workdir; nil means observation was incomplete.
func watchWrites(ctx context.Context, workdir, top string) func() (changed, touched *[]string) {
	unobserved := func() (*[]string, *[]string) { return nil, nil }
	roots, plain := writeRoots(ctx, workdir, top)
	if len(roots)+len(plain) == 0 {
		return unobserved
	}
	before, err := snapTrees(ctx, roots, nil)
	if err != nil {
		return unobserved
	}
	beforePlain := treeSnap{}
	if plain != "" {
		if beforePlain, err = snapPlain(plain); err != nil {
			return unobserved
		}
	}
	return func() (*[]string, *[]string) {
		after, err := snapTrees(ctx, roots, before)
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
		if plain != "" {
			afterPlain, err := snapPlain(plain)
			if err != nil {
				return nil, nil
			}
			changed = append(changed, diffPlain(beforePlain, afterPlain)...)
		}
		if len(touched) == 0 {
			return capPaths(changed), nil
		}
		return capPaths(changed), capPaths(touched)
	}
}

// writeRoots returns the repos to watch and, when workdir sits in no repo, workdir itself as a plain directory.
func writeRoots(ctx context.Context, workdir, top string) ([]string, string) {
	root := top
	if !treeHolds(root, workdir) { // a resumed or re-run job can carry another directory's tree
		gctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writesTimeout)
		root = paths.GitTop(gctx, workdir)
		cancel()
	}
	var roots []string
	if root != "" {
		roots = append(roots, root)
	}
	if root != "" {
		return roots, ""
	}
	return roots, plainDir(workdir)
}

// plainDir is workdir (the cwd when empty) as an absolute real directory, or "" when it is none.
func plainDir(workdir string) string {
	abs, err := filepath.Abs(workdir)
	if err != nil {
		return ""
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return ""
	}
	if fi, err := os.Stat(real); err != nil || !fi.IsDir() {
		return ""
	}
	return real
}

// plainCap bounds a plain directory's entries; a bigger one (a home, /tmp) stays unobserved rather than guessed at.
const plainCap = 5000

var errPlainTooBig = errors.New("directory too large to observe")

// snapPlain is the signature of every file under a non-repo root.
func snapPlain(root string) (treeSnap, error) {
	snap, seen := treeSnap{}, 0
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // a file vanishing mid-walk is a change, not a failure
		}
		if seen++; seen > plainCap {
			return errPlainTooBig
		}
		if d.IsDir() {
			return nil
		}
		snap.add(p)
		return nil
	})
	return snap, err
}

// diffPlain lists the paths added, removed or rewritten between two plain snapshots.
func diffPlain(before, after treeSnap) []string {
	var out []string
	for p, sig := range after {
		if was, ok := before[p]; !ok || was != sig {
			out = append(out, p)
		}
	}
	for p := range before {
		if _, ok := after[p]; !ok {
			out = append(out, p)
		}
	}
	return out
}

func snapTrees(ctx context.Context, roots []string, also treeSnap) (treeSnap, error) {
	snap := treeSnap{}
	for _, root := range roots {
		part, err := snapTree(ctx, root, nil)
		if err != nil {
			return nil, err
		}
		maps.Copy(snap, part)
	}
	for p := range also {
		snap.add(p)
	}
	return snap, nil
}

func capPaths(list []string) *[]string {
	slices.Sort(list)
	if len(list) > writesCap {
		list = append(list[:writesCap], WroteTruncated)
	}
	return &list
}

// dropSharedWrites clears Wrote when a peer shared the run's tree or plain dir: its writes are not this run's.
func dropSharedWrites(rec *Record) {
	w := rec.Wrote
	if w == nil || len(*w) == 0 {
		return
	}
	switch {
	case sharesTree(*rec, time.Now()):
		rec.Wrote, rec.TreeChanged = nil, w
	case sharesCwd(*rec, time.Now()):
		rec.Wrote = nil
	}
}

// sharesTree reports whether another dispatch in rec's git tree overlapped rec's run, which ended at end.
func sharesTree(rec Record, end time.Time) bool {
	return rec.Tree != "" && sharesDir(rec, end, rec.Tree, func(o Record) bool { return o.Tree == rec.Tree })
}

// sharesCwd reports whether another dispatch started in rec's non-repo directory overlapped rec's run.
func sharesCwd(rec Record, end time.Time) bool {
	return rec.Tree == "" && rec.Cwd != "" && sharesDir(rec, end, rec.Cwd, func(o Record) bool { return o.Tree == "" && o.Cwd == rec.Cwd })
}

// sharesDir scans records mentioning needle for a same(o) peer whose run overlapped rec's.
func sharesDir(rec Record, end time.Time, needle string, same func(Record) bool) bool {
	shared := false
	err := eachRecent(paths.DispatchDir(), jsonToken(needle), func(o Record) bool {
		if o.ID == rec.ID || !same(o) {
			return true
		}
		oEnd := o.Time.Add(time.Duration(o.DurationMs) * time.Millisecond)
		if o.Alive() {
			oEnd = end
		}
		shared = o.Time.Before(end) && oEnd.After(rec.Time)
		return !shared
	})
	return err == nil && shared
}
