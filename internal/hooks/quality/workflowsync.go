package quality

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const workflowSyncUsage = "usage: claude-code workflow sync <canonical.js>\nExits: 0 synced or unchanged; 1 file/marker error; 2 usage.\n"

// RunWorkflowSync copies canonical helper blocks into sibling scripts without editing the canonical file.
func RunWorkflowSync(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		if _, err := io.WriteString(stdout, workflowSyncUsage); err == nil {
			return 0
		}
		return 1
	}
	if len(args) != 1 || strings.HasPrefix(args[0], "-") || filepath.Ext(args[0]) != ".js" {
		fmt.Fprint(stderr, workflowSyncUsage)
		return 2
	}
	canonical, err := os.ReadFile(args[0])
	if err != nil {
		fmt.Fprintf(stderr, "workflow sync: %v\n", err)
		return 1
	}
	if _, err = syncHelperBlocks(string(canonical), string(canonical)); err != nil {
		fmt.Fprintf(stderr, "workflow sync: %s: %v\n", args[0], err)
		return 1
	}
	return syncWorkflowDir(args[0], string(canonical), stdout, stderr)
}

func syncWorkflowDir(path, canonical string, stdout, stderr io.Writer) int {
	dir := filepath.Dir(path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		fmt.Fprintf(stderr, "workflow sync: %v\n", err)
		return 1
	}
	code := 0
	for _, entry := range entries {
		if entry.Name() == filepath.Base(path) || filepath.Ext(entry.Name()) != ".js" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		status, err := syncWorkflowFile(path, canonical)
		if err != nil {
			fmt.Fprintf(stderr, "workflow sync: %s: %v\n", path, err)
			code = 1
			continue
		}
		if _, err := fmt.Fprintf(stdout, "%s %s\n", status, path); err != nil {
			fmt.Fprintf(stderr, "workflow sync: writing output: %v\n", err)
			return 1
		}
	}
	return code
}

func syncWorkflowFile(path, canonical string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("not a regular script")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	updated, err := syncHelperBlocks(canonical, string(data))
	if err != nil {
		return "", err
	}
	if updated == string(data) {
		return "unchanged", nil
	}
	if err := os.WriteFile(path, []byte(updated), info.Mode().Perm()); err != nil {
		return "", err
	}
	return "synced", nil
}

func syncHelperBlocks(canonical, target string) (string, error) {
	for _, block := range helperBlocks {
		ci, cj, err := syncBlockBounds(canonical, block)
		if err != nil {
			return "", fmt.Errorf("canonical: %w", err)
		}
		ti, tj, err := syncBlockBounds(target, block)
		if err != nil {
			return "", err
		}
		if ci < 0 {
			if ti >= 0 {
				return "", fmt.Errorf("canonical: no %s block", block.name)
			}
			continue
		}
		if ti >= 0 {
			target = target[:ti] + canonical[ci:cj] + target[tj:]
		}
	}
	return target, nil
}

func syncBlockBounds(src string, block helperBlock) (int, int, error) {
	i, j := strings.Index(src, block.start), strings.Index(src, block.end)
	if i < 0 && j < 0 && !usesAny(src, block.uses) {
		return -1, -1, nil
	}
	if i < 0 || j < i {
		return 0, 0, fmt.Errorf("no %s block (%s … %s)", block.name, block.start, block.end)
	}
	return i, j + len(block.end), nil
}
