package worktree

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/platform/paths"
)

// liveMerge is one landed path dirty in the live tree: the three-way merge of its live bytes with the lane's change.
type liveMerge struct {
	path     string
	data     []byte
	mode     os.FileMode
	conflict bool
}

// mergeDirtyPaths merges the lane's change into each landed path the live tree has edits in, keeping both sides.
func mergeDirtyPaths(ctx context.Context, main, from, to string, files []string) ([]liveMerge, error) {
	dirty, err := targetDirtyPaths(ctx, main)
	if err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp("", "land-merge-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	var out []liveMerge
	for _, f := range files {
		if !dirty[f] {
			continue
		}
		m, err := mergeOne(ctx, main, tmp, from, to, f)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// mergeOne is git merge-file over the live file, its bytes at from and its bytes at to; a missing side reads as empty.
func mergeOne(ctx context.Context, main, tmp, from, to, rel string) (liveMerge, error) {
	live := filepath.Join(main, filepath.FromSlash(rel))
	m := liveMerge{path: rel}
	ours, mode, err := readLive(live, rel)
	m.mode = mode
	if err != nil {
		return m, err
	}
	args, err := writeSides(tmp, [][]byte{ours, blobOrEmpty(ctx, main, from, rel), blobOrEmpty(ctx, main, to, rel)})
	if err != nil {
		return m, err
	}
	cmd := gitCmd(ctx, "merge-file", "-p", "-L", "live tree", "-L", "base", "-L", "lane", args[0], args[1], args[2])
	m.data, err = cmd.Output()
	if code, isExit := asExitErr(err); isExit && code > 0 && code < 128 {
		m.conflict, err = true, nil // the exit status counts the conflicts; the merged text is still complete
	}
	if err != nil {
		return m, fmt.Errorf("git merge-file %s: %v; nothing changed: %w", rel, err, ErrLandRefused)
	}
	return m, nil
}

// readLive reads the live file and its mode (0644 when absent); a non-regular file is refused.
func readLive(live, rel string) ([]byte, os.FileMode, error) {
	mode := os.FileMode(0o644)
	if fi, err := os.Lstat(live); err == nil {
		if !fi.Mode().IsRegular() {
			return nil, mode, fmt.Errorf("%s is not a regular file in the live tree; nothing changed: %w", rel, ErrLandRefused)
		}
		mode = fi.Mode().Perm()
	}
	ours, err := os.ReadFile(live)
	if err != nil && !os.IsNotExist(err) {
		return nil, mode, err
	}
	return ours, mode, nil
}

// writeSides writes each side to its own file under tmp and returns the three paths.
func writeSides(tmp string, sides [][]byte) ([3]string, error) {
	var args [3]string
	for i, side := range sides {
		args[i] = filepath.Join(tmp, strconv.Itoa(i))
		if err := os.WriteFile(args[i], side, 0o600); err != nil {
			return args, err
		}
	}
	return args, nil
}

// blobOrEmpty is path's bytes at rev, or none when rev has no such file.
func blobOrEmpty(ctx context.Context, dir, rev, path string) []byte {
	out, err := runGit(ctx, dir, nil, "cat-file", "blob", rev+":"+path)
	if err != nil {
		return nil
	}
	return out
}

// excludeArgs are git apply --exclude flags naming rels literally (glob characters escaped).
func excludeArgs(merges []liveMerge) []string {
	esc := strings.NewReplacer(`\`, `\\`, "*", `\*`, "?", `\?`, "[", `\[`)
	args := make([]string, len(merges))
	for i, m := range merges {
		args[i] = "--exclude=" + esc.Replace(m.path)
	}
	return args
}

// writeMerges replaces each merged live file in place with its merged bytes, keeping its mode.
func writeMerges(main string, merges []liveMerge) error {
	for _, m := range merges {
		dest := filepath.Join(main, filepath.FromSlash(m.path))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		if err := paths.AtomicWrite(dest, m.data, m.mode); err != nil {
			return fmt.Errorf("write merged %s: %w", m.path, err)
		}
	}
	return nil
}

// noteMerges records on res which landed paths were merged into live edits and which carry conflict markers.
func noteMerges(res *LandResult, merges []liveMerge) {
	for _, m := range merges {
		if m.conflict {
			res.Conflicted = append(res.Conflicted, m.path)
		} else {
			res.Merged = append(res.Merged, m.path)
		}
	}
}
