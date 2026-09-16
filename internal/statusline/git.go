package statusline

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// gitBranch reads .git/HEAD directly (a per-render fork costs more than the render); falls back to git.
func gitBranch(cwd string) string {
	if cwd == "" {
		cwd = "."
	}
	if branch, ok := gitBranchFast(cwd); ok {
		return branch
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", cwd, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// gitBranchFast walks up to .git (one gitfile indirection; detached → "HEAD"). ok=false → ask git;
// ("", true) → no repo, without a fork.
func gitBranchFast(cwd string) (string, bool) {
	if os.Getenv("GIT_DIR") != "" {
		return "", false
	}
	dir, err := filepath.Abs(cwd)
	if err != nil {
		return "", false
	}
	for {
		gitPath := filepath.Join(dir, ".git")
		if fi, err := os.Stat(gitPath); err == nil {
			gitDir := gitPath
			if !fi.IsDir() {
				raw, err := os.ReadFile(gitPath)
				if err != nil {
					return "", false
				}
				target := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(raw)), "gitdir:"))
				if target == "" {
					return "", false
				}
				if !filepath.IsAbs(target) {
					target = filepath.Join(dir, target)
				}
				gitDir = target
			}
			head, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
			if err != nil {
				return "", false
			}
			ref := strings.TrimSpace(string(head))
			if rest, isRef := strings.CutPrefix(ref, "ref: "); isRef {
				if branch, isHead := strings.CutPrefix(rest, "refs/heads/"); isHead {
					return branch, true
				}
				return "", false
			}
			return "HEAD", true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", true
		}
		dir = parent
	}
}

// A slow git (network FS, huge index) must degrade to no velocity, not stall the bar.
const gitDiffTimeout = 700 * time.Millisecond

// Bound the untracked sweep so a stray build dir can't charge thousands of reads to a render.
const (
	untrackedFileCap = 300
	untrackedByteCap = 2 << 20
)

// treeDiffStat counts uncommitted lines (tracked vs HEAD + untracked); ok=false means "no repo".
func treeDiffStat(cwd string) (adds, dels int, ok bool) {
	if cwd == "" {
		cwd = "."
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitDiffTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", cwd, "--no-optional-locks", "diff", "--numstat",
		"--no-ext-diff", "--no-textconv", "HEAD").Output()
	if err != nil {
		return 0, 0, false
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.SplitN(line, "\t", 3)
		if len(f) < 3 {
			continue
		}
		a, aerr := strconv.Atoi(f[0])
		d, derr := strconv.Atoi(f[1])
		if aerr != nil || derr != nil { // binary files report "-"
			continue
		}
		adds += a
		dels += d
	}
	return adds + untrackedLines(ctx, cwd), dels, true
}

func untrackedLines(ctx context.Context, cwd string) int {
	out, err := exec.CommandContext(ctx, "git", "-C", cwd, "ls-files", "--others", "--exclude-standard", "-z").Output()
	if err != nil {
		return 0
	}
	total, seen := 0, 0
	for _, rel := range strings.Split(string(out), "\x00") {
		if rel == "" {
			continue
		}
		if seen++; seen > untrackedFileCap {
			break
		}
		total += fileLines(filepath.Join(cwd, rel))
	}
	return total
}

// fileLines counts lines of a small text file; binary (NUL in the head) or oversized counts zero.
func fileLines(path string) int {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() || fi.Size() > untrackedByteCap || fi.Size() == 0 {
		return 0
	}
	b, err := os.ReadFile(path)
	if err != nil || bytes.IndexByte(b[:min(len(b), 8192)], 0) >= 0 {
		return 0
	}
	return bytes.Count(bytes.TrimSuffix(b, []byte("\n")), []byte("\n")) + 1
}
