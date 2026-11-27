package paths

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/edotau/claude-code/internal/platform/proc"
)

// Slug lowercases s into one safe path element
func Slug(s, keep string, max int, fallback string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', strings.ContainsRune(keep, r):
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
	}
	slug := strings.Trim(b.String(), "-")
	if max > 0 && len(slug) > max {
		slug = strings.Trim(slug[:max], "-")
	}
	if slug == "" {
		return fallback
	}
	return slug
}

// ProjectSlug mirrors Claude Code's projects/<slug> naming: every non-alphanumeric byte becomes '-'.
func ProjectSlug(root string) string {
	b := []byte(filepath.Clean(root))
	for i, c := range b {
		if ('a' > c || c > 'z') && ('A' > c || c > 'Z') && ('0' > c || c > '9') {
			b[i] = '-'
		}
	}
	return string(b)
}

// HomeRepo names the work outside any git repo; '@' never survives repoPart, so no repo can claim it.
const HomeRepo = "@home"

// Repo is a checkout's name, the owner a clash qualifies it with, and the key (origin URL, else path) owning it.
type Repo struct{ Name, Owner, Key string }

// RepoName names root's git repo: origin's last path element, else the main checkout's folder; HomeRepo outside git.
func RepoName(root string) Repo {
	if root == "" {
		return Repo{Name: HomeRepo}
	}
	top, main := CheckoutOf(root)
	if top == "" {
		return Repo{Name: HomeRepo}
	}
	url, ok := originURLFile(main)
	if !ok {
		url, _ = proc.RunGit(context.Background(), main, "config", "--get", "remote.origin.url")
	}
	if key, owner, name := remoteParts(strings.TrimSpace(url)); name != "" {
		return Repo{Name: name, Owner: owner, Key: key}
	}
	main = Canon(main) // one key per checkout, whichever symlinked spelling reached it
	return Repo{Name: repoPart(filepath.Base(main)), Owner: repoPart(filepath.Base(filepath.Dir(main))), Key: main}
}

// originURLFile reads origin's url from main's plain .git/config; ok=false (ask git) for any layout it cannot decide.
func originURLFile(main string) (url string, ok bool) {
	raw, err := os.ReadFile(filepath.Join(main, ".git", "config"))
	if err != nil || bytes.Contains(bytes.ToLower(raw), []byte("include")) {
		return "", false // a .git file, a missing config, or an include/includeIf git would follow
	}
	if _, err := os.Stat(filepath.Join(main, ".git", "reftable")); err == nil {
		return "", false
	}
	return configOriginURL(string(raw))
}

// configOriginURL is the last url of the [remote "origin"] section; ok=false when git must parse it.
func configOriginURL(raw string) (url string, ok bool) {
	in := false
	for l := range strings.SplitSeq(raw, "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "[") {
			in = l == `[remote "origin"]`
			if !in && strings.HasPrefix(strings.ToLower(l), "[remote") && !strings.HasPrefix(l, `[remote "`) {
				return "", false // an old-style or unusual remote section header
			}
			continue
		}
		if k, v, cut := strings.Cut(l, "="); in && cut && strings.EqualFold(strings.TrimSpace(k), "url") {
			if v = strings.TrimSpace(v); strings.ContainsAny(v, "\"\\#;") {
				return "", false // quoting, escapes or a comment: let git parse it
			}
			url = v
		}
	}
	return url, url != ""
}

// remoteParts splits an https, ssh, scp-style or local-path remote URL into a normalized key, owner and repo name.
func remoteParts(url string) (key, owner, name string) {
	if url == "" {
		return "", "", ""
	}
	s, host := hostPath(url)
	s = strings.TrimSuffix(strings.Trim(filepath.ToSlash(s), "/"), ".git")
	parts := strings.Split(s, "/")
	if host && len(parts) < 2 {
		return "", "", "" // a bare host names no repo
	}
	name = repoPart(parts[len(parts)-1])
	if len(parts) > 1 {
		owner = repoPart(parts[len(parts)-2])
	}
	return strings.ToLower(s), owner, name
}

// hostPath is url as host/path without scheme or user (host false for a local path).
func hostPath(url string) (s string, host bool) {
	if _, rest, ok := strings.Cut(url, "://"); ok {
		s = rest
	} else if i := strings.IndexAny(url, ":/\\"); i > 0 && url[i] == ':' && !filepath.IsAbs(url) {
		s = url[:i] + "/" + url[i+1:] // scp form host:owner/repo
	} else {
		return url, false
	}
	if at := strings.LastIndex(s, "@"); at >= 0 && at < strings.IndexAny(s+"/", "/") {
		s = s[at+1:] // user@host
	}
	return s, true
}

// repoPart sanitises one name element; leading dots go so no name is hidden or ".."
func repoPart(s string) string {
	return strings.TrimLeft(Slug(strings.TrimSuffix(s, ".git"), "._-", 0, ""), ".-")
}

// GitTop is cwd's `git rev-parse --show-toplevel`, "" when git fails or prints nothing (not a repo).
func GitTop(ctx context.Context, cwd string) string {
	out, err := proc.RunGit(ctx, cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// MainCheckout resolves a worktree pointer lexically; with a git runner it probes enclosing repos, failing to "".
func MainCheckout(root string, gitOut ...func(string, ...string) (string, error)) string {
	if len(gitOut) > 0 {
		out, err := gitOut[0](root, "rev-parse", "--path-format=absolute", "--show-toplevel", "--git-common-dir")
		if err != nil {
			return ""
		}
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if len(lines) != 2 || lines[0] == "" {
			return ""
		}
		top, common := filepath.Clean(lines[0]), filepath.Clean(lines[1])
		if filepath.Base(common) == ".git" {
			return filepath.Dir(common)
		}
		return top
	}
	data, err := os.ReadFile(filepath.Join(root, ".git"))
	if err != nil {
		return root
	}
	ptr, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir:")
	if !ok {
		return root
	}
	ptr = strings.TrimSpace(ptr)
	if !filepath.IsAbs(ptr) {
		ptr = filepath.Join(root, ptr)
	}
	ptr = filepath.Clean(ptr)
	marker := string(filepath.Separator) + filepath.Join(".git", "worktrees") + string(filepath.Separator)
	if i := strings.Index(ptr, marker); i > 0 {
		return ptr[:i]
	}
	return root
}

// CheckoutOf walks up from dir to the nearest directory holding .git
func CheckoutOf(dir string) (top, main string) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", ""
	}
	for {
		if hasGit(filepath.Join(dir, ".git")) {
			return dir, MainCheckout(dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", ""
		}
		dir = parent
	}
}

func hasGit(path string) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	if !fi.IsDir() {
		return true
	}
	if _, err := os.Lstat(filepath.Join(path, "HEAD")); err == nil {
		return true // a repo's .git always holds HEAD, so skip the open+readdir
	}
	d, err := os.Open(path)
	if err != nil {
		return false
	}
	defer d.Close()
	names, _ := d.Readdirnames(1)
	return len(names) > 0
}

// FoldsCase holds where the default filesystem is case-insensitive (macOS, Windows); Linux compares byte-exact.
const FoldsCase = runtime.GOOS != "linux"

// FoldCase lowercases p where FoldsCase holds, for a caller that matches a glob or prefix itself.
func FoldCase(p string) string {
	if !FoldsCase {
		return p
	}
	return strings.ToLower(p)
}

// Real is p made absolute and symlink-resolved through its deepest existing ancestor (see CanonDeep).
func Real(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		p = a
	}
	return CanonDeep(p)
}

// SameFile reports whether a and b name one file: both Real, then case-folded per FoldsCase.
func SameFile(a, b string) bool { return FoldCase(Real(a)) == FoldCase(Real(b)) }

// InsideDir reports whether p is dir or under it, compared as SameFile compares.
func InsideDir(p, dir string) bool { return Within(FoldCase(Real(p)), FoldCase(Real(dir))) }

// SamePath compares two paths after Clean and symlink resolution (unresolvable paths compare cleaned).
func SamePath(a, b string) bool {
	return Canon(a) == Canon(b)
}

// Canon cleans p and resolves its symlinks; an unresolvable path (e.g. already deleted) stays cleaned.
func Canon(p string) string {
	p = filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// LaneRoot is ~/worktrees/<main's basename without a leading dot> under the canonical home.
func LaneRoot(main string) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(Canon(home), "worktrees", strings.TrimPrefix(filepath.Base(filepath.Clean(main)), "."))
}

const maxLinkDepth = 40

// CanonDeep resolves symlinks in p's deepest existing ancestor and rejoins the missing tail unresolved
func CanonDeep(p string) string { return canonDeep(p, 0) }

func canonDeep(p string, depth int) string {
	p = filepath.Clean(p)
	tail := ""
	for cur := p; ; {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(r, tail)
		}
		if t, ok := danglingTarget(cur, depth); ok {
			return filepath.Join(canonDeep(t, depth+1), tail)
		}
		if cur, tail = existingAncestor(cur, tail); cur == "" {
			return p
		}
	}
}

// existingAncestor climbs past missing ancestors with Lstat, sparing EvalSymlinks' full walk per level; "" at root.
func existingAncestor(cur, tail string) (string, string) {
	for {
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", tail
		}
		tail = filepath.Join(filepath.Base(cur), tail)
		if _, err := os.Lstat(parent); err == nil {
			return parent, tail
		}
		cur = parent
	}
}

// danglingTarget returns the absolute target of cur when it is a symlink whose target is missing.
func danglingTarget(cur string, depth int) (string, bool) {
	if depth >= maxLinkDepth {
		return "", false
	}
	if fi, err := os.Lstat(cur); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		return "", false
	}
	t, err := os.Readlink(cur)
	if err != nil {
		return "", false
	}
	if !filepath.IsAbs(t) {
		t = filepath.Join(canonDeep(filepath.Dir(cur), depth+1), t)
	}
	return t, true
}
