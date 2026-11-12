package statusline

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
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
	out, err := proc.RunGitInherit(ctx, cwd, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// gitBranchFast walks up to .git (one gitfile hop; detached → "HEAD"); ok=false → ask git; ("", true) → no repo.
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
				var ok bool
				gitDir, ok = gitfileTarget(gitPath)
				if !ok {
					return "", false
				}
			}
			return gitHeadBranch(gitDir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", true
		}
		dir = parent
	}
}

// checkoutOf finds cwd's checkout: root is the main checkout (a subdir or linked worktree maps to the repo it
// belongs to; no repo → cwd) and worktree is the linked worktree's top dir, "" in the main checkout.
func checkoutOf(cwd string) (root, worktree string) {
	top, main := paths.CheckoutOf(cwd)
	switch {
	case top == "":
		return cwd, ""
	case main != top:
		return main, top
	}
	return top, ""
}

func gitfileTarget(path string) (string, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	target := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(raw)), "gitdir:"))
	if target == "" {
		return "", false
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(path), target)
	}
	return target, true
}

func gitHeadBranch(gitDir string) (string, bool) {
	head, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return "", false
	}
	ref := strings.TrimSpace(string(head))
	if rest, isRef := strings.CutPrefix(ref, "ref: "); isRef {
		branch, isHead := strings.CutPrefix(rest, "refs/heads/")
		if !isHead {
			return "", false
		}
		return branch, true
	}
	return "HEAD", true
}

// A slow git (network FS, huge index) must degrade to no velocity, not stall the bar.
var gitDiffTimeout = 700 * time.Millisecond

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
	untrackedDone := make(chan int, 1)
	go func() { untrackedDone <- untrackedLines(ctx, cwd) }()
	out, err := numstat(ctx, cwd, "HEAD")
	if err != nil && ctx.Err() == nil && inWorkTree(ctx, cwd) { // a fresh repo has no HEAD: diff against the empty tree
		out, err = numstat(ctx, cwd, emptyTree)
	}
	if err != nil {
		cancel()
	}
	untracked := <-untrackedDone
	if err != nil {
		return 0, 0, false
	}
	adds, dels = sumNumstat(out)
	if ctx.Err() != nil { // deadline spent mid-sweep: no partial count, the caller falls back to the ledger
		return 0, 0, false
	}
	return adds + untracked, dels, true
}

// sumNumstat totals the added and deleted lines of `git diff --numstat` output, skipping binary files.
func sumNumstat(out []byte) (adds, dels int) {
	for line := range strings.SplitSeq(string(out), "\n") {
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
	return adds, dels
}

// emptyTree is git's well-known empty tree object, the base for a repo with no commits yet.
const emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

func numstat(ctx context.Context, cwd, base string) ([]byte, error) {
	out, err := proc.RunGitInherit(ctx, cwd, "--no-optional-locks", "diff", "--numstat", "--no-ext-diff", "--no-textconv", base)
	return []byte(out), err
}

func inWorkTree(ctx context.Context, cwd string) bool {
	_, err := proc.RunGitInherit(ctx, cwd, "rev-parse", "--is-inside-work-tree")
	return err == nil
}

// untrackedLines counts untracked lines, stopping early once ctx expires.
func untrackedLines(ctx context.Context, cwd string) int {
	out, err := proc.RunGitInherit(ctx, cwd, "ls-files", "--others", "--exclude-standard", "-z", "--", ":/")
	if err != nil {
		return 0
	}
	total, seen := 0, 0
	for rel := range strings.SplitSeq(out, "\x00") {
		if rel == "" {
			continue
		}
		if seen++; seen > untrackedFileCap || ctx.Err() != nil {
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
