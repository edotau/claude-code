// Package pack inlines tracked workspace files into one prompt under a byte budget; never .env*, state/ or keys.
package pack

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/proc"
)

// Collection defaults: the limit is the total byte budget; DefaultMaxFiles is a safety cap --max-files overrides.
const (
	DefaultMaxFiles     = 5000
	DefaultMaxFileBytes = 128 * 1024 // 128 KiB per file
)

// ignoredSegments: any path containing one of these segments is skipped.
var ignoredSegments = map[string]bool{
	".git": true, ".next": true, ".turbo": true,
	"build": true, "coverage": true, "dist": true, "node_modules": true,
}

// IsTestPath reports whether rel (relative to a walked dir) is test code or fixture data in a common language shape.
func IsTestPath(rel string) bool {
	segs := strings.Split(rel, "/")
	for _, seg := range segs[:len(segs)-1] {
		if seg == "testdata" || seg == "__tests__" {
			return true
		}
	}
	base := segs[len(segs)-1]
	name := strings.TrimSuffix(base, filepath.Ext(base))
	switch strings.ToLower(filepath.Ext(base)) {
	case ".go", ".py":
		return strings.HasSuffix(name, "_test") || (filepath.Ext(base) == ".py" && strings.HasPrefix(name, "test_"))
	case ".ts", ".js":
		return strings.HasSuffix(name, ".test") || strings.HasSuffix(name, ".spec")
	}
	return false
}

// binaryExtensions: files with these extensions are never inlined (also skipped on a NUL byte).
var binaryExtensions = map[string]bool{
	".7z": true, ".ai": true, ".avif": true, ".bmp": true, ".class": true, ".db": true, ".dll": true,
	".dylib": true, ".eot": true, ".exe": true, ".gif": true, ".gz": true, ".ico": true, ".jar": true,
	".jpeg": true, ".jpg": true, ".lockb": true, ".mov": true, ".mp3": true, ".mp4": true, ".otf": true,
	".pdf": true, ".png": true, ".pyc": true, ".so": true, ".svgz": true, ".tar": true, ".ttf": true,
	".wasm": true, ".webm": true, ".webp": true, ".woff": true, ".woff2": true, ".zip": true,
}

// mediaTypes maps an extension to the media_type label shown in the prompt inventory.
var mediaTypes = map[string]string{
	".csv": "text/csv", ".graphql": "application/graphql", ".gql": "application/graphql",
	".html": "text/html", ".json": "application/json", ".jsonl": "application/x-ndjson",
	".md": "text/markdown", ".sql": "text/sql", ".toml": "application/toml",
	".tsv": "text/tab-separated-values", ".xml": "application/xml",
	".yaml": "application/yaml", ".yml": "application/yaml",
}

// secretExtensions: files with these extensions are never inlined, regardless of git tracking.
var secretExtensions = map[string]bool{
	".pem": true, ".key": true, ".p12": true, ".pfx": true, ".keystore": true, ".jks": true, ".tfstate": true, ".tfvars": true,
}

// secretNames: exact lower-case basenames that hold credentials; secretDirs: directories whose files do.
var secretNames = map[string]bool{
	".npmrc": true, ".netrc": true, "_netrc": true, ".git-credentials": true, ".pypirc": true, ".htpasswd": true,
	"kubeconfig": true, "service-account.json": true, "token.txt": true, "secret.yaml": true, "secret.yml": true,
	"secrets.yaml": true, "secrets.yml": true, "secret.json": true, "secrets.json": true,
}
var secretDirs = map[string]bool{".kube": true, ".docker": true, "state": true}

// IncludedFile is one inlined file's content + metadata for the prompt.
type IncludedFile struct {
	Path      string // relative to cwd, forward slashes
	MediaType string
	Bytes     int // original length
	Truncated bool
	Content   string
}

// SkippedFile records why a matched path was not inlined.
type SkippedFile struct {
	Path   string
	Reason string
}

// Context is the collected file payload for a prompt.
type Context struct {
	Included []IncludedFile
	Skipped  []SkippedFile
}

// isRegularFile reports whether path exists and is a regular file.
func isRegularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// isSymlink reports whether path itself is a symbolic link (not followed).
func isSymlink(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode()&os.ModeSymlink != 0
}

// Collect inlines up to maxFiles files (0 none, <0 default) of dirs and patterns, each cut at maxFileBytes, in budget;
// withTests=false skips test files found by walking dirs (files matched by patterns are kept).
func Collect(ctx context.Context, cwd string, dirs, patterns []string, maxFiles, maxFileBytes, budget int, withTests bool) (Context, error) {
	if err := ctx.Err(); err != nil {
		return Context{}, err
	}
	if maxFiles < 0 {
		maxFiles = DefaultMaxFiles
	}
	if maxFileBytes <= 0 {
		maxFileBytes = DefaultMaxFileBytes
	}

	matches, skipped, err := matchPaths(ctx, cwd, dirs, patterns, withTests)
	if err != nil {
		return Context{}, err
	}

	c := collector{cwd: realPath(cwd), maxFiles: maxFiles, maxFileBytes: maxFileBytes, budget: budget}
	c.out.Skipped = append(c.out.Skipped, skipped...)
	for _, abs := range sortedMatches(cwd, matches) {
		if err := ctx.Err(); err != nil {
			return c.out, err
		}
		if err := c.add(abs, relSlash(cwd, abs)); err != nil {
			return c.out, err
		}
	}
	return c.out, nil
}

// matchPaths walks dirs and globs patterns into the deduped match set, plus what the walk and globs skipped.
func matchPaths(ctx context.Context, cwd string, dirs, patterns []string, withTests bool) (map[string]bool, []SkippedFile, error) {
	tracked, repoRoots, err := gitTrackedFiles(ctx, cwd, gitRoots(cwd, dirs, patterns))
	if err != nil {
		return nil, nil, err
	}
	// Each root decides its own gitignore filtering: a git root filters, a non-git root does not.
	matches := map[string]bool{}
	var skipped []SkippedFile
	for _, dir := range dirs {
		dirMatches, dirSkipped, err := walkDir(ctx, cwd, dir, tracked, repoRoots[absIn(cwd, dir)], withTests)
		if err != nil {
			return nil, nil, err
		}
		for p := range dirMatches {
			matches[p] = true
		}
		skipped = append(skipped, dirSkipped...)
	}
	for _, pattern := range patterns {
		skipped = append(skipped, globPattern(cwd, pattern, matches, tracked, repoRoots[globBase(cwd, pattern)])...)
	}
	return matches, skipped, nil
}

// sortedMatches orders matched paths by path, then stably by groupRank.
func sortedMatches(cwd string, matches map[string]bool) []string {
	sorted := make([]string, 0, len(matches))
	for p := range matches {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted) // stable path order within a group, applied next
	sort.SliceStable(sorted, func(i, j int) bool {
		return groupRank(relSlash(cwd, sorted[i])) < groupRank(relSlash(cwd, sorted[j]))
	})
	return sorted
}

// collector accumulates inlined and skipped files against the file-count and byte budgets.
type collector struct {
	out                    Context
	totalBytes, budget     int
	maxFiles, maxFileBytes int
	cwd                    string // symlink-resolved working dir
}

func (c *collector) skip(rel, reason string) {
	c.out.Skipped = append(c.out.Skipped, SkippedFile{rel, reason})
}

// resolvesToSecret reports whether abs, with symlinks resolved, is secret-like relative to the real cwd.
func (c *collector) resolvesToSecret(abs string) bool {
	return SecretLike(relSlash(c.cwd, realPath(abs)))
}

// preSkipReason is why rel (at abs) is skipped before reading it, or "".
func (c *collector) preSkipReason(rel, abs string) string {
	switch {
	case isIgnored(rel):
		return "ignored-path"
	case SecretLike(rel) || c.resolvesToSecret(abs):
		return "secret-like"
	case c.maxFiles == 0:
		return "index-only"
	case len(c.out.Included) >= c.maxFiles:
		return "max-files-exceeded"
	case c.totalBytes >= c.budget:
		return "budget-exceeded"
	}
	return ""
}

// add inlines abs, or records why it was skipped.
func (c *collector) add(abs, rel string) error {
	if reason := c.preSkipReason(rel, abs); reason != "" {
		c.skip(rel, reason)
		return nil
	}
	inc, reason, err := readInclude(abs, rel, c.maxFileBytes)
	if err != nil {
		return err
	}
	switch {
	case reason != "":
		c.skip(rel, reason)
	case c.totalBytes+len(inc.Content) > c.budget:
		c.skip(rel, "budget-exceeded")
	default:
		c.totalBytes += len(inc.Content)
		c.out.Included = append(c.out.Included, inc)
	}
	return nil
}

// groupRank orders inlining candidates: source (0), tests (1), testdata (2), so a budget cutoff keeps real source.
func groupRank(rel string) int {
	for seg := range strings.SplitSeq(rel, "/") {
		if seg == "testdata" || seg == "fixtures" {
			return 2
		}
	}
	base := filepath.Base(rel)
	name := strings.TrimSuffix(base, filepath.Ext(base))
	if strings.HasSuffix(name, "_test") || strings.HasPrefix(name, "test_") {
		return 1
	}
	return 0
}

// walkDir collects files under dir, pruning ignored dirs and, inside a git work tree, anything gitignored.
func walkDir(ctx context.Context, cwd, dir string, tracked map[string]bool, inRepo, withTests bool) (map[string]bool, []SkippedFile, error) {
	matches := map[string]bool{}
	var skipped []SkippedFile
	root := dir
	if !filepath.IsAbs(root) {
		root = filepath.Join(cwd, dir)
	}
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			skipped = append(skipped, SkippedFile{relSlash(cwd, p), "walk-error: " + err.Error()})
			return nil // unreadable entry: skip, don't abort the walk
		}
		if d.IsDir() {
			// Prune here, not per-file; a pruned directory is not a file, so it is not counted as skipped.
			if p != root && ignoredSegments[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		rel := relSlash(cwd, p)
		if reason := walkSkipReason(d, rel, relSlash(root, p), tracked, inRepo, withTests); reason != "" {
			skipped = append(skipped, SkippedFile{rel, reason})
			return nil
		}
		matches[p] = true
		return nil
	})
	if walkErr != nil { // the callback only ever returns a non-nil error on cancellation
		return nil, nil, walkErr
	}
	return matches, skipped, nil
}

// walkSkipReason is why a walked file is not inlined ("" keeps it); inRoot is its path inside the walked dir.
func walkSkipReason(d fs.DirEntry, rel, inRoot string, tracked map[string]bool, inRepo, withTests bool) string {
	switch {
	case !d.Type().IsRegular():
		return "non-regular-file"
	case inRepo && !tracked[rel]:
		return "gitignored"
	case !withTests && IsTestPath(inRoot):
		return "tests-excluded"
	}
	return ""
}

// globPattern expands pattern into matches (mutated), returning glob-error and filtered skips; gitignored hits drop.
func globPattern(cwd, pattern string, matches map[string]bool, tracked map[string]bool, inRepo bool) []SkippedFile {
	glob := pattern
	if !filepath.IsAbs(glob) {
		glob = filepath.Join(globEscape(cwd), pattern) // a cwd holding [, * or ? must match literally
	}
	hits, err := filepath.Glob(glob)
	if err != nil {
		return []SkippedFile{{pattern, "glob-error: " + err.Error()}}
	}
	var skipped []SkippedFile
	for _, h := range hits {
		rel := relSlash(cwd, h)
		switch {
		case isSymlink(h), escapesCwd(cwd, rel, h):
			skipped = append(skipped, SkippedFile{rel, "symlink"})
		case !isRegularFile(h):
		case inRepo && !tracked[rel]:
			skipped = append(skipped, SkippedFile{rel, "gitignored"})
		default:
			matches[h] = true
		}
	}
	return skipped
}

// escapesCwd reports whether h, resolved through symlinked directories, lands outside cwd; h sits under cwd by rel.
func escapesCwd(cwd, rel, h string) bool {
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return false // an explicit outside path was asked for by name
	}
	realCwd, err1 := filepath.EvalSymlinks(cwd)
	realHit, err2 := filepath.EvalSymlinks(h)
	if err1 != nil || err2 != nil {
		return false
	}
	back, err := filepath.Rel(realCwd, realHit)
	return err != nil || back == ".." || strings.HasPrefix(back, "../")
}

// gitTrackedFiles maps each root's cached and untracked-kept paths to cwd-relative keys; repoRoots marks roots in git.
func gitTrackedFiles(ctx context.Context, cwd string, roots []string) (map[string]bool, map[string]bool, error) {
	tracked := map[string]bool{}
	repoRoots := map[string]bool{}
	for _, root := range roots {
		inside, err := gitListRoot(ctx, cwd, root, tracked)
		if err != nil {
			return nil, nil, err
		}
		if inside {
			repoRoots[root] = true
		}
	}
	return tracked, repoRoots, nil
}

// gitListRoot adds root's cached + untracked-not-ignored paths to tracked; inside=false outside a work tree.
func gitListRoot(ctx context.Context, cwd, root string, tracked map[string]bool) (inside bool, err error) {
	if inside, err = insideWorkTree(ctx, root); !inside || err != nil {
		return false, err
	}
	out, err := proc.RunGitRaw(ctx, root, "ls-files", "--cached", "--others", "--exclude-standard", "-z")
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, ctxErr // cancellation stays context.Canceled, not a ls-files failure
		}
		return false, fmt.Errorf("git ls-files in %s: %w", root, err) // fail closed: never walk gitignored files
	}
	for p := range strings.SplitSeq(strings.Trim(out, "\x00"), "\x00") {
		if p != "" {
			tracked[relSlash(cwd, filepath.Join(root, p))] = true
		}
	}
	return true, nil
}

// insideWorkTree reports whether root is a git work tree; a missing root or a confirmed non-repo is false, no error.
func insideWorkTree(ctx context.Context, root string) (bool, error) {
	if info, serr := os.Stat(root); serr != nil || !info.IsDir() {
		return false, nil // a missing root surfaces as a walk or glob error, not a git one
	}
	_, rerr := proc.RunGitRaw(ctx, root, "rev-parse", "--is-inside-work-tree")
	switch {
	case rerr == nil:
		return true, nil
	case ctx.Err() != nil:
		return false, ctx.Err()
	case notARepo(root, rerr):
		return false, nil
	}
	return false, fmt.Errorf("git rev-parse in %s: %w", root, rerr)
}

// gitRoots lists the dirs whose git listing decides inlining: each requested dir and each pattern's literal base.
func gitRoots(cwd string, dirs, patterns []string) []string {
	roots := make([]string, 0, len(dirs)+len(patterns))
	for _, d := range dirs {
		roots = append(roots, absIn(cwd, d))
	}
	for _, p := range patterns {
		roots = append(roots, globBase(cwd, p))
	}
	slices.Sort(roots)
	return slices.Compact(roots) // --diff yields one pattern per file: list each base once
}

// globBase is the directory a pattern starts in, before its first glob metacharacter, as an absolute path.
func globBase(cwd, pattern string) string {
	dir := filepath.Dir(pattern)
	if hasGlobMeta(pattern) {
		dir = pattern
		for hasGlobMeta(dir) {
			dir = filepath.Dir(dir)
		}
	}
	return absIn(cwd, dir)
}

// hasGlobMeta reports whether p contains a filepath.Match metacharacter.
func hasGlobMeta(p string) bool {
	return strings.ContainsAny(p, `*?[\`)
}

// absIn returns p as an absolute path, resolving a relative p against cwd.
func absIn(cwd, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(cwd, p)
}

// realPath resolves symlinks in p, falling back to p itself when it cannot be resolved.
func realPath(p string) string {
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	return p
}

// notARepo reports a confirmed non-repo: no .git in or above cwd AND git's exit 128 "not a git repository".
func notARepo(cwd string, rerr error) bool {
	if gitEntryAbove(cwd) {
		return false // a .git exists but git rejects it: a broken repo, so fail closed
	}
	var ee *exec.ExitError
	return errors.As(rerr, &ee) && ee.ExitCode() == 128 && strings.Contains(rerr.Error(), "not a git repository")
}

// gitEntryAbove reports whether a .git (directory or file) exists in dir or any parent; true on an unresolvable path.
func gitEntryAbove(dir string) bool {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return true
	}
	for {
		if _, err := os.Lstat(filepath.Join(abs, ".git")); err == nil {
			return true
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return false
		}
		abs = parent
	}
}

// readInclude bounded-reads abs; returns the IncludedFile or a skip reason (stat/open/read failures → read-error).
func readInclude(abs, rel string, maxFileBytes int) (IncludedFile, string, error) {
	info, err := os.Stat(abs)
	if err != nil {
		return IncludedFile{}, "read-error: " + err.Error(), nil
	}
	if !info.Mode().IsRegular() {
		return IncludedFile{}, "non-regular-file", nil
	}
	if binaryExtensions[strings.ToLower(filepath.Ext(abs))] {
		return IncludedFile{}, "unsupported-extension", nil
	}
	f, err := os.Open(abs)
	if err != nil {
		return IncludedFile{}, "read-error: " + err.Error(), nil
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, int64(maxFileBytes)+1))
	if err != nil {
		return IncludedFile{}, "read-error: " + err.Error(), nil
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return IncludedFile{}, "binary-content", nil
	}
	truncated := info.Size() > int64(maxFileBytes)
	content := data
	if truncated && len(content) > maxFileBytes {
		content = trimPartialRune(content[:maxFileBytes])
	}
	return IncludedFile{
		Path: rel, MediaType: mediaType(abs), Bytes: int(info.Size()),
		Truncated: truncated, Content: string(content),
	}, "", nil
}

// trimPartialRune drops a multi-byte rune cut off at the end of b.
func trimPartialRune(b []byte) []byte {
	for i := 1; i < utf8.UTFMax && i <= len(b); i++ {
		if utf8.RuneStart(b[len(b)-i]) {
			if utf8.FullRune(b[len(b)-i:]) {
				return b
			}
			return b[:len(b)-i]
		}
	}
	return b
}

// relSlash returns target relative to cwd with forward slashes (falls back to the abs path on error).
func relSlash(cwd, target string) string {
	rel, err := filepath.Rel(cwd, target)
	if err != nil {
		rel = target
	}
	return filepath.ToSlash(rel)
}

// FilePrefix names the pack files a run stages in its workdir for an agent that reads them; Collect never inlines one.
const FilePrefix = ".claude-code-pack-"

// isIgnored reports whether any path segment is in ignoredSegments or is a staged pack file.
func isIgnored(rel string) bool {
	for seg := range strings.SplitSeq(rel, "/") {
		if ignoredSegments[seg] || strings.HasPrefix(seg, FilePrefix) {
			return true
		}
	}
	return false
}

// SecretLike: rel names an env, secrets or credentials file, a key/cert, or sits under state/; never sent to an agent.
func SecretLike(rel string) bool {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	base := strings.ToLower(parts[len(parts)-1])
	for _, prefix := range []string{".env", "credentials", "id_rsa", "id_ed25519"} {
		if strings.HasPrefix(base, prefix) {
			return true
		}
	}
	if base == "secrets.env" || secretNames[base] || secretExtensions[filepath.Ext(base)] || isSSHKeyName(base) {
		return true
	}
	if strings.Contains(base, ".tfstate.") {
		return true
	}
	return slices.ContainsFunc(parts, func(seg string) bool { return secretDirs[strings.ToLower(seg)] })
}

// isSSHKeyName reports an OpenSSH private key default name (id_rsa, id_ed25519, ...), not its .pub.
func isSSHKeyName(base string) bool {
	return strings.HasPrefix(base, "id_") && filepath.Ext(base) == ""
}

// mediaType maps a path's extension to a media_type label (default text/plain).
func mediaType(path string) string {
	if mt, ok := mediaTypes[strings.ToLower(filepath.Ext(path))]; ok {
		return mt
	}
	return "text/plain"
}

// globEscape backslash-escapes filepath.Match metacharacters so s matches itself.
func globEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `*`, `\*`, `?`, `\?`, `[`, `\[`).Replace(s)
}
