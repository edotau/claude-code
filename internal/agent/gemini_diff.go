package agent

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// ChangedFilesVsHEAD lists tracked files changed against HEAD (staged + unstaged + untracked), which is
// what a review run is actually about — reviewing the whole tree buries the diff. An error from either
// git command (not found, no HEAD, not a repo, …) is returned with its stderr instead of being masked.
func ChangedFilesVsHEAD(ctx context.Context, root string) ([]string, error) {
	var all []string
	for _, args := range [][]string{
		// --relative matches ls-files's cwd-relative output, so a subdirectory run doesn't mix bases.
		{"-C", root, "diff", "--name-only", "--relative", "HEAD"},
		{"-C", root, "ls-files", "--others", "--exclude-standard"},
	} {
		out, err := exec.CommandContext(ctx, "git", args...).Output()
		if err != nil {
			stderr := stderrOf(err)
			if ctx.Err() != nil {
				err = ctx.Err()
			}
			return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, stderr)
		}
		all = append(all, strings.Split(strings.TrimSpace(string(out)), "\n")...)
	}
	seen := map[string]bool{}
	var uniq []string
	for _, p := range all {
		if p = strings.TrimSpace(p); p != "" && !seen[p] {
			seen[p] = true
			uniq = append(uniq, p)
		}
	}
	return uniq, nil
}

// stderrOf extracts a *exec.ExitError's trimmed stderr, or empty for any other error type.
func stderrOf(err error) string {
	if ee, ok := err.(*exec.ExitError); ok {
		return strings.TrimSpace(string(ee.Stderr))
	}
	return ""
}
