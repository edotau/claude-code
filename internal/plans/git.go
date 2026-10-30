package plans

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// CommitFiles lists the files sha changed in the repo at dir.
func CommitFiles(ctx context.Context, dir, sha string) ([]string, error) {
	raw, err := exec.CommandContext(ctx, "git", "-C", dir, "show", "-z", "--name-only", "--format=", sha).Output()
	if err != nil {
		return nil, fmt.Errorf("git show %s: %w", sha, err)
	}
	var files []string
	for f := range strings.SplitSeq(string(raw), "\x00") {
		if f = strings.TrimPrefix(f, "\n"); f != "" {
			files = append(files, f)
		}
	}
	return files, nil
}

// RangeCommits lists rng's non-merge commits oldest first, each with a 7-char sha and its changed files.
func RangeCommits(ctx context.Context, dir, rng string) ([]Commit, error) {
	if !strings.Contains(rng, "..") || strings.HasPrefix(rng, "-") {
		return nil, fmt.Errorf("range %q: want <a>..<b>", rng)
	}
	raw, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-list", "--reverse", "--no-merges", rng, "--").Output()
	if err != nil {
		return nil, fmt.Errorf("git rev-list %s: %w", rng, err)
	}
	var commits []Commit
	for sha := range strings.FieldsSeq(string(raw)) {
		files, err := CommitFiles(ctx, dir, sha)
		if err != nil {
			return nil, err
		}
		commits = append(commits, Commit{Sha: sha[:7], Files: files})
	}
	return commits, nil
}
