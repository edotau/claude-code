// Package gemini assembles workspace file context into one structured prompt for the gemini bridge.
package gemini

import (
	"bytes"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// ModelEnvKeys are consulted in order when no explicit model is passed.
var ModelEnvKeys = []string{"CLAUDE_CODE_GEMINI_MODEL", "GEMINI_MODEL"}

// Collection defaults (ported from the JS bridge).
const (
	DefaultMaxFiles     = 40
	DefaultMaxFileBytes = 32768
)

// ignoredSegments: any path containing one of these segments is skipped.
var ignoredSegments = map[string]bool{
	".git": true, ".next": true, ".turbo": true,
	"build": true, "coverage": true, "dist": true, "node_modules": true,
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
var secretExtensions = map[string]bool{".pem": true, ".key": true, ".p12": true, ".pfx": true}

// ResolveModel picks a model: explicit arg → first set env in envKeys → def.
func ResolveModel(arg string, envKeys []string, def string) string {
	if arg != "" {
		return arg
	}
	for _, key := range envKeys {
		if v := os.Getenv(key); v != "" {
			return v
		}
	}
	return def
}

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

// CollectContext walks dirs and globs patterns, inlining up to maxFiles text files (0 = none, <0 = default),
// each cut at maxFileBytes (<=0 = default); matches are deduped and sorted so output is deterministic.
func CollectContext(cwd string, dirs, patterns []string, maxFiles, maxFileBytes int) (Context, error) {
	if maxFiles < 0 {
		maxFiles = DefaultMaxFiles
	}
	if maxFileBytes <= 0 {
		maxFileBytes = DefaultMaxFileBytes
	}

	tracked, inRepo := gitTrackedFiles(cwd)
	matches, skipped := walkDirs(cwd, dirs, tracked, inRepo)
	skipped = append(skipped, globPatterns(cwd, patterns, matches)...)

	sorted := make([]string, 0, len(matches))
	for p := range matches {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)

	var ctx Context
	ctx.Skipped = append(ctx.Skipped, skipped...)
	for _, abs := range sorted {
		rel := relSlash(cwd, abs)
		switch {
		case isIgnored(rel):
			ctx.Skipped = append(ctx.Skipped, SkippedFile{rel, "ignored-path"})
		case isSecretLike(rel):
			ctx.Skipped = append(ctx.Skipped, SkippedFile{rel, "secret-like"})
		case len(ctx.Included) >= maxFiles:
			ctx.Skipped = append(ctx.Skipped, SkippedFile{rel, "max-files-exceeded"})
		default:
			inc, reason, err := readInclude(abs, rel, maxFileBytes)
			if err != nil {
				return ctx, err
			}
			if reason != "" {
				ctx.Skipped = append(ctx.Skipped, SkippedFile{rel, reason})
			} else {
				ctx.Included = append(ctx.Included, inc)
			}
		}
	}
	return ctx, nil
}

// walkDirs recursively collects files under dirs, pruning ignored directories and — inside a git work
// tree — anything not cached or untracked-but-not-ignored (gitignored paths are never inlined).
func walkDirs(cwd string, dirs []string, tracked map[string]bool, inRepo bool) (map[string]bool, []SkippedFile) {
	matches := map[string]bool{}
	var skipped []SkippedFile
	for _, dir := range dirs {
		root := dir
		if !filepath.IsAbs(root) {
			root = filepath.Join(cwd, dir)
		}
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				skipped = append(skipped, SkippedFile{relSlash(cwd, p), "walk-error: " + err.Error()})
				return nil // unreadable entry: skip, don't abort the walk
			}
			if d.IsDir() {
				// Prune here, not per-file: walking into .git/node_modules/etc. would report every file inside.
				if p != root && ignoredSegments[d.Name()] {
					skipped = append(skipped, SkippedFile{relSlash(cwd, p), "ignored-path"})
					return fs.SkipDir
				}
				return nil
			}
			rel := relSlash(cwd, p)
			if inRepo && !tracked[rel] {
				skipped = append(skipped, SkippedFile{rel, "gitignored"})
				return nil
			}
			matches[p] = true
			return nil
		})
	}
	return matches, skipped
}

// globPatterns expands patterns into matches (mutated in place), returning any glob-error skips.
func globPatterns(cwd string, patterns []string, matches map[string]bool) []SkippedFile {
	var skipped []SkippedFile
	for _, pattern := range patterns {
		glob := pattern
		if !filepath.IsAbs(glob) {
			glob = filepath.Join(cwd, pattern)
		}
		hits, err := filepath.Glob(glob)
		if err != nil {
			skipped = append(skipped, SkippedFile{pattern, "glob-error: " + err.Error()})
			continue
		}
		for _, h := range hits {
			if isRegularFile(h) {
				matches[h] = true
			}
		}
	}
	return skipped
}

// gitTrackedFiles lists cwd's cached + untracked-not-ignored paths (relative, forward-slash) when cwd
// is inside a git work tree; ok is false outside one, and the caller then walks everything.
func gitTrackedFiles(cwd string) (tracked map[string]bool, ok bool) {
	if exec.Command("git", "-C", cwd, "rev-parse", "--is-inside-work-tree").Run() != nil {
		return nil, false
	}
	out, err := exec.Command("git", "-C", cwd, "ls-files", "--cached", "--others", "--exclude-standard", "-z").Output()
	if err != nil {
		return nil, false
	}
	tracked = map[string]bool{}
	for _, p := range strings.Split(strings.Trim(string(out), "\x00"), "\x00") {
		if p != "" {
			tracked[filepath.ToSlash(p)] = true
		}
	}
	return tracked, true
}

// readInclude stats and bounded-reads abs (at most maxFileBytes+1 bytes), returning either the
// IncludedFile, a non-empty skip reason, or an error if even the stat/open failed.
func readInclude(abs, rel string, maxFileBytes int) (IncludedFile, string, error) {
	info, err := os.Stat(abs)
	if err != nil {
		return IncludedFile{}, "read-error: " + err.Error(), nil
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
	if isBinary(abs, data) {
		return IncludedFile{}, "unsupported-extension", nil
	}
	truncated := info.Size() > int64(maxFileBytes)
	content := data
	if truncated && len(content) > maxFileBytes {
		content = content[:maxFileBytes]
	}
	return IncludedFile{
		Path: rel, MediaType: mediaType(abs), Bytes: int(info.Size()),
		Truncated: truncated, Content: string(content),
	}, "", nil
}

// relSlash returns target relative to cwd with forward slashes (falls back to the abs path on error).
func relSlash(cwd, target string) string {
	rel, err := filepath.Rel(cwd, target)
	if err != nil {
		rel = target
	}
	return filepath.ToSlash(rel)
}

// isIgnored reports whether any path segment is in ignoredSegments.
func isIgnored(rel string) bool {
	for _, seg := range strings.Split(rel, "/") {
		if ignoredSegments[seg] {
			return true
		}
	}
	return false
}

// isBinary reports a binary file: known binary extension or a NUL byte in the content.
func isBinary(path string, data []byte) bool {
	if binaryExtensions[strings.ToLower(filepath.Ext(path))] {
		return true
	}
	return bytes.IndexByte(data, 0) >= 0
}

// isSecretLike reports whether rel names an env file, a secrets file, a key/cert, or lives under an
// env.d/state directory — never inlined even when explicitly matched by --files.
func isSecretLike(rel string) bool {
	parts := strings.Split(rel, "/")
	base := parts[len(parts)-1]
	if base == ".env" || strings.HasPrefix(base, ".env.") || base == "secrets.env" {
		return true
	}
	if secretExtensions[strings.ToLower(filepath.Ext(base))] {
		return true
	}
	for _, seg := range parts {
		if seg == "env.d" || seg == "state" {
			return true
		}
	}
	return false
}

// mediaType maps a path's extension to a media_type label (default text/plain).
func mediaType(path string) string {
	if mt, ok := mediaTypes[strings.ToLower(filepath.Ext(path))]; ok {
		return mt
	}
	return "text/plain"
}
