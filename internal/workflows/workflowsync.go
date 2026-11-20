package workflows

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const workflowSyncUsage = "usage: claude-code workflow sync [<root>]\nExits: 0 synced or unchanged; 1 file/marker error; 2 usage.\n"

// RunWorkflowSync writes embedded helper blocks into marked workflow scripts under root.
func RunWorkflowSync(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		if _, err := io.WriteString(stdout, workflowSyncUsage); err == nil {
			return 0
		}
		return 1
	}
	if len(args) > 1 || (len(args) == 1 && strings.HasPrefix(args[0], "-")) {
		fmt.Fprint(stderr, workflowSyncUsage)
		return 2
	}
	root := "."
	if len(args) == 1 {
		root = args[0]
	}
	return syncWorkflowDir(root, stdout, stderr)
}

func syncWorkflowDir(root string, stdout, stderr io.Writer) int {
	scripts := lintTargets(root)
	if len(scripts) == 0 {
		fmt.Fprintf(stderr, "workflow sync: no scripts in %s\n", filepath.Join(root, "workflows", "*.js"))
		return 1
	}
	code := 0
	for _, path := range scripts {
		status, err := syncWorkflowFile(path)
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

func syncWorkflowFile(path string) (string, error) {
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
	updated, err := syncHelperBlocks(string(data))
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

func syncHelperBlocks(target string) (string, error) {
	for _, block := range helperBlocks {
		ti, tj, err := syncBlockBounds(target, block)
		if err != nil {
			return "", err
		}
		if ti >= 0 {
			target = target[:ti] + block.start + block.source + block.end + target[tj:]
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
		return 0, 0, fmt.Errorf("no %s block (%s … %s)", block.name, strings.TrimSpace(block.start), block.end)
	}
	if strings.Count(src, block.start) != 1 || strings.Count(src, block.end) != 1 {
		return 0, 0, fmt.Errorf("duplicate %s block markers", block.name)
	}
	return i, j + len(block.end), nil
}
