package statusline

import (
	"bytes"
	"cmp"
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

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

// gitBranchFast reads HEAD from the nearest gitdir (detached → "HEAD"); ok=false → ask git; ("", true) → no repo.
func gitBranchFast(cwd string) (string, bool) {
	gitDir, inRepo, ok := gitDirOf(cwd)
	if !ok {
		return "", false
	}
	if !inRepo {
		return "", true
	}
	return gitHeadBranch(gitDir)
}

// gitDirOf walks up to .git (one gitfile hop); ok=false → ask git; inRepo=false (ok) → no repo.
func gitDirOf(cwd string) (gitDir string, inRepo, ok bool) {
	if os.Getenv("GIT_DIR") != "" {
		return "", false, false
	}
	dir, err := filepath.Abs(cwd)
	if err != nil {
		return "", false, false
	}
	for {
		gitPath := filepath.Join(dir, ".git")
		if fi, err := os.Stat(gitPath); err == nil {
			if fi.IsDir() {
				return gitPath, true, true
			}
			target, ok := gitfileTarget(gitPath)
			return target, ok, ok
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false, true
		}
		dir = parent
	}
}

// checkoutOf finds cwd's checkout: root is the main checkout (no repo: cwd), worktree the linked top dir or "".
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

// headReadCap bounds the HEAD read; a real HEAD is one short line.
const headReadCap = 4 << 10

func gitHeadBranch(gitDir string) (string, bool) {
	f, err := os.Open(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return "", false
	}
	defer f.Close()
	head, err := io.ReadAll(io.LimitReader(f, headReadCap+1))
	if err != nil || len(head) > headReadCap {
		return "", false
	}
	ref := strings.TrimSpace(string(head))
	if !printableRef(ref) {
		return "", false
	}
	if rest, isRef := strings.CutPrefix(ref, "ref: "); isRef {
		branch, isHead := strings.CutPrefix(rest, "refs/heads/")
		if !isHead || branch == ".invalid" { // reftable repos park HEAD on refs/heads/.invalid: ask git
			return "", false
		}
		return branch, true
	}
	return "HEAD", true
}

// printableRef rejects invalid UTF-8 and control characters so HEAD never writes escapes to the terminal.
func printableRef(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	return !strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

// A slow git (network FS, huge index) must degrade to no velocity, not stall the bar.
var gitDiffTimeout = 700 * time.Millisecond

// Bound the untracked sweep so a stray build dir can't charge thousands of reads to a render.
const (
	untrackedFileCap = 300
	untrackedByteCap = 2 << 20
)

// treeDiffStat counts the branch's lines (committed + uncommitted vs its base, + untracked) and names the base's
// kind ("pin", "fork", "up" or ""); ok=false means "no repo".
func treeDiffStat(cwd string) (adds, dels int, kind string, ok bool) {
	if cwd == "" {
		cwd = "."
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitDiffTimeout)
	defer cancel()
	untrackedDone := make(chan int, 1)
	go func() { untrackedDone <- untrackedLines(ctx, cwd) }()
	base, kind := branchBase(ctx, cwd)
	out, err := numstat(ctx, cwd, cmp.Or(base, "HEAD"))
	if err != nil && ctx.Err() == nil && inWorkTree(ctx, cwd) { // a fresh repo has no HEAD: diff against the empty tree
		out, err = numstat(ctx, cwd, emptyTree)
	}
	if err != nil {
		cancel()
	}
	untracked := <-untrackedDone
	if err != nil {
		return 0, 0, "", false
	}
	adds, dels = sumNumstat(out)
	if ctx.Err() != nil { // deadline spent mid-sweep: no partial count, the caller falls back to the ledger
		return 0, 0, "", false
	}
	return adds + untracked, dels, kind, true
}

// branchBase is the diff base and its kind: a claudeBase pin, the claudeForkPoint, a non-default upstream, else the
// merge-base with the default branch (or the upstream on the default); "" if unknown.
func branchBase(ctx context.Context, cwd string) (base, kind string) {
	branch := gitBranch(cwd)
	if branch != "" && branch != "HEAD" {
		pin, fork := branchClaudeRefs(ctx, cwd, branch)
		for _, c := range [...]struct{ ref, kind string }{{pin, "pin"}, {fork, "fork"}} {
			if sha := resolveCommit(ctx, cwd, c.ref); sha != "" {
				return sha, c.kind
			}
		}
	}
	def := defaultBranch(ctx, cwd)
	if def == "" {
		return "", ""
	}
	if strings.TrimPrefix(def, "origin/") == branch {
		return gitLine(ctx, cwd, "rev-parse", "--verify", "--quiet", "@{upstream}"), ""
	}
	up := gitLine(ctx, cwd, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}")
	if up != "" && strings.TrimPrefix(up, "origin/") != strings.TrimPrefix(def, "origin/") {
		if sha := resolveCommit(ctx, cwd, up); sha != "" {
			return sha, "up"
		}
	}
	return gitLine(ctx, cwd, "merge-base", "HEAD", def), ""
}

// branchClaudeRefs reads the branch's claudeBase and claudeForkPoint config in one git call; "" when unset.
func branchClaudeRefs(ctx context.Context, cwd, branch string) (pin, fork string) {
	pattern := "^branch\\." + regexp.QuoteMeta(branch) + "\\.claude(base|forkpoint)$"
	for line := range strings.SplitSeq(gitLine(ctx, cwd, "config", "--get-regexp", pattern), "\n") {
		key, val, _ := strings.Cut(line, " ")
		switch {
		case strings.HasSuffix(key, ".claudebase"):
			pin = val
		case strings.HasSuffix(key, ".claudeforkpoint"):
			fork = val
		}
	}
	return pin, fork
}

// resolveCommit is ref's commit sha; "" when ref is empty or no longer resolves.
func resolveCommit(ctx context.Context, cwd, ref string) string {
	if ref == "" || strings.HasPrefix(ref, "-") {
		return ""
	}
	return gitLine(ctx, cwd, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
}

// defaultBranch is origin/HEAD's target (e.g. "origin/main"), else a local main or master; "" if none.
func defaultBranch(ctx context.Context, cwd string) string {
	if ref, ok := originHeadFast(cwd); ok {
		return ref
	}
	if ref := gitLine(ctx, cwd, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"); ref != "" {
		return ref
	}
	for _, name := range []string{"main", "master"} {
		if gitLine(ctx, cwd, "rev-parse", "--verify", "--quiet", "refs/heads/"+name) != "" {
			return name
		}
	}
	return ""
}

// originHeadFast is `symbolic-ref --short refs/remotes/origin/HEAD` read from the common dir; ok=false → ask git.
func originHeadFast(cwd string) (string, bool) {
	gitDir, inRepo, ok := gitDirOf(cwd)
	if !ok || !inRepo || os.Getenv("GIT_COMMON_DIR") != "" {
		return "", false
	}
	common := commonDir(gitDir)
	head, err := os.ReadFile(filepath.Join(common, "refs", "remotes", "origin", "HEAD"))
	if err != nil || len(head) > headReadCap {
		return "", false
	}
	short, ok := remoteShortName(strings.TrimSpace(string(head)))
	if !ok {
		return "", false
	}
	for _, other := range []string{"heads", "tags"} { // --short keeps a longer name when these exist
		if _, err := os.Lstat(filepath.Join(common, "refs", other, filepath.FromSlash(short))); err == nil {
			return "", false
		}
	}
	if packedMentions(common, short) {
		return "", false
	}
	return short, true
}

// packedMentions reports whether packed-refs holds a branch or tag named short; an unreadable file counts as yes.
func packedMentions(common, short string) bool {
	data, err := os.ReadFile(filepath.Join(common, "packed-refs"))
	if err != nil {
		return !os.IsNotExist(err)
	}
	for line := range strings.Lines(string(data)) {
		line = strings.TrimRight(line, "\r\n")
		if strings.HasSuffix(line, " refs/heads/"+short) || strings.HasSuffix(line, " refs/tags/"+short) {
			return true
		}
	}
	return false
}

// commonDir is where a linked worktree's gitdir keeps shared refs (its commondir file), else gitDir itself.
func commonDir(gitDir string) string {
	raw, err := os.ReadFile(filepath.Join(gitDir, "commondir"))
	if err != nil {
		return gitDir
	}
	if common := strings.TrimSpace(string(raw)); filepath.IsAbs(common) {
		return common
	}
	return filepath.Join(gitDir, strings.TrimSpace(string(raw)))
}

// remoteShortName is "origin/main" for "ref: refs/remotes/origin/main"; ok=false for any other content.
func remoteShortName(ref string) (string, bool) {
	rest, isRef := strings.CutPrefix(ref, "ref: ")
	short, isRemote := strings.CutPrefix(rest, "refs/remotes/")
	return short, isRef && isRemote && short != "" && printableRef(ref)
}

// gitLine is the trimmed stdout of a read-only git call; "" on any failure.
func gitLine(ctx context.Context, cwd string, args ...string) string {
	out, err := proc.RunGitInherit(ctx, cwd, append([]string{"--no-optional-locks"}, args...)...)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
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
	fi, err := os.Lstat(path) // Lstat: a symlink to an outside file is not untracked lines
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > untrackedByteCap || fi.Size() == 0 {
		return 0
	}
	b, err := os.ReadFile(path)
	if err != nil || bytes.IndexByte(b[:min(len(b), 8192)], 0) >= 0 {
		return 0
	}
	return bytes.Count(bytes.TrimSuffix(b, []byte("\n")), []byte("\n")) + 1
}
