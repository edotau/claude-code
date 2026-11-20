// Write record of a dispatch: the files its repository changed between the start and the end of its run.

package agent

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
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

// watchWrites observes the workdir and allowed write roots; nil means observation was incomplete.
func watchWrites(ctx context.Context, workdir, top string, allow ...Rule) func() (changed, touched *[]string) {
	unobserved := func() (*[]string, *[]string) { return nil, nil }
	roots, ok := writeRoots(ctx, workdir, top, allow)
	if !ok {
		return unobserved
	}
	before, err := snapTrees(ctx, roots, nil)
	if err != nil {
		return unobserved
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
		if len(touched) == 0 {
			return capPaths(changed), nil
		}
		return capPaths(changed), capPaths(touched)
	}
}

func writeRoots(ctx context.Context, workdir, top string, allow []Rule) ([]string, bool) {
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
	extras, ok := allowedWriteRoots(ctx, workdir, allow)
	if !ok {
		return nil, false
	}
	for _, extra := range extras {
		if !slices.Contains(roots, extra) {
			roots = append(roots, extra)
		}
	}
	return roots, len(roots) > 0
}

func allowedWritePath(workdir, arg string) string {
	arg = expandHome("Write", arg)
	if arg == "" || strings.HasPrefix(arg, "~") {
		return ""
	}
	if !filepath.IsAbs(arg) {
		base, err := filepath.Abs(workdir)
		if err != nil {
			return ""
		}
		arg = filepath.Join(base, arg)
	}
	return arg
}

func allowedWriteRoots(ctx context.Context, workdir string, allow []Rule) ([]string, bool) {
	var roots []string
	byDir := map[string]string{}
	byRoot := map[string][]string{}
	for _, r := range allow {
		if r.Tool != "Edit" && r.Tool != "Write" {
			continue
		}
		anchor, dir := allowedWriteAnchor(workdir, r.Arg)
		if dir == "" {
			return nil, false
		}
		root := byDir[dir]
		if root == "" {
			gctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writesTimeout)
			root = paths.GitTop(gctx, dir)
			cancel()
			byDir[dir] = root
		}
		if root == "" {
			return nil, false
		}
		if _, ok := byRoot[root]; !ok {
			roots = append(roots, root)
		}
		byRoot[root] = append(byRoot[root], anchor, anchor+string(filepath.Separator))
	}
	for _, root := range roots {
		if !unignoredWrites(ctx, root, byRoot[root]) {
			return nil, false
		}
	}
	return roots, true
}

func allowedWriteAnchor(workdir, arg string) (string, string) {
	arg = allowedWritePath(workdir, arg)
	if arg == "" {
		return "", ""
	}
	anchor := arg
	if i := strings.IndexAny(arg, "*?["); i >= 0 {
		anchor = filepath.Dir(arg[:i] + "_")
	}
	dir := anchor
	for {
		fi, err := os.Stat(dir)
		if err == nil && fi.IsDir() {
			break
		}
		if err != nil && !os.IsNotExist(err) {
			return "", ""
		}
		dir = filepath.Dir(dir)
	}
	return anchor, dir
}

func unignoredWrites(ctx context.Context, root string, anchors []string) bool {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writesTimeout)
	defer cancel()
	_, err := proc.RunGitRaw(ctx, root, append([]string{"check-ignore", "--"}, anchors...)...)
	var exit *exec.ExitError
	return errors.As(err, &exit) && exit.ExitCode() == 1
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

// sharesTree reports whether another dispatch in rec's git tree overlapped rec's run, which ended at end.
func sharesTree(rec Record, end time.Time) bool {
	if rec.Tree == "" {
		return false
	}
	shared := false
	err := eachRecent(paths.DispatchDir(), jsonToken(rec.Tree), func(o Record) bool {
		if o.ID == rec.ID || o.Tree != rec.Tree {
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
