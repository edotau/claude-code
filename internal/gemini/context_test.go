package gemini

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// write creates path (and its parent dirs) with body, mode 0644.
func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestResolveModel: explicit arg wins; default when nothing set; GEMINI_MODEL fallback;
// CLAUDE_CODE_GEMINI_MODEL beats GEMINI_MODEL.
func TestResolveModel(t *testing.T) {
	t.Setenv("CLAUDE_CODE_GEMINI_MODEL", "")
	t.Setenv("GEMINI_MODEL", "")

	if got := ResolveModel("explicit-model", ModelEnvKeys, "def-model"); got != "explicit-model" {
		t.Errorf("explicit arg: got %q", got)
	}
	if got := ResolveModel("", ModelEnvKeys, "def-model"); got != "def-model" {
		t.Errorf("default: got %q, want %q", got, "def-model")
	}

	t.Setenv("GEMINI_MODEL", "gemini-env-model")
	if got := ResolveModel("", ModelEnvKeys, "def-model"); got != "gemini-env-model" {
		t.Errorf("env fallback: got %q", got)
	}

	t.Setenv("CLAUDE_CODE_GEMINI_MODEL", "gemini-primary-model")
	if got := ResolveModel("", ModelEnvKeys, "def-model"); got != "gemini-primary-model" {
		t.Errorf("env precedence: got %q", got)
	}
}

// TestCollectContext: globs + walks, skips ignored dirs, binary, and honors caps + truncation.
func TestCollectContext(t *testing.T) {
	dir := t.TempDir()
	w := func(rel, content string) { write(t, filepath.Join(dir, rel), content) }
	w("a.go", "package a")
	w("sub/b.go", "package b")
	w("node_modules/pkg/index.js", "ignored")
	w("bin.bin", "\x00\x01binary")
	w("big.txt", strings.Repeat("x", 100))

	ctx, err := CollectContext(dir, []string{"."}, nil, 40, 10)
	if err != nil {
		t.Fatal(err)
	}

	included := map[string]IncludedFile{}
	for _, f := range ctx.Included {
		included[f.Path] = f
	}
	if _, ok := included["a.go"]; !ok {
		t.Error("a.go should be included")
	}
	if _, ok := included["sub/b.go"]; !ok {
		t.Error("sub/b.go should be included (recursive walk)")
	}

	skipReasons := map[string]string{}
	for _, s := range ctx.Skipped {
		skipReasons[s.Path] = s.Reason
	}
	// The directory is pruned at walk time, not enumerated file-by-file — one Skipped entry for the
	// whole subtree, not one per file inside it.
	if skipReasons["node_modules"] != "ignored-path" {
		t.Errorf("node_modules should be ignored-path, got %q", skipReasons["node_modules"])
	}
	if _, ok := skipReasons["node_modules/pkg/index.js"]; ok {
		t.Error("files inside a pruned dir must not appear individually in Skipped")
	}
	// bin.bin has a NUL byte → unsupported-extension.
	if r := skipReasons["bin.bin"]; r != "unsupported-extension" {
		t.Errorf("bin.bin should be unsupported-extension, got %q", r)
	}
	// big.txt exceeds maxFileBytes=10 → truncated, original Bytes preserved.
	if f, ok := included["big.txt"]; !ok {
		t.Error("big.txt should be included")
	} else if !f.Truncated || len(f.Content) != 10 || f.Bytes != 100 {
		t.Errorf("big.txt truncation wrong: truncated=%t len=%d bytes=%d", f.Truncated, len(f.Content), f.Bytes)
	}
}

// TestCollectContextMaxFiles: max-files cap marks the overflow skipped.
func TestCollectContextMaxFiles(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a.txt", "b.txt", "c.txt"} {
		write(t, filepath.Join(dir, n), "x")
	}
	ctx, err := CollectContext(dir, []string{"."}, nil, 2, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(ctx.Included) != 2 {
		t.Errorf("included = %d, want 2 (cap)", len(ctx.Included))
	}
	found := false
	for _, s := range ctx.Skipped {
		if s.Reason == "max-files-exceeded" {
			found = true
		}
	}
	if !found {
		t.Error("expected a max-files-exceeded skip")
	}
}

// TestCollectContextGlobError: a malformed --pattern is recorded as Skipped, not silently dropped.
func TestCollectContextGlobError(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.txt"), "x")

	// "[" is an unterminated character class: filepath.Glob returns ErrBadPattern for it.
	ctx, err := CollectContext(dir, nil, []string{"["}, 40, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range ctx.Skipped {
		if s.Path == "[" && strings.HasPrefix(s.Reason, "glob-error: ") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a glob-error Skipped entry for pattern %q, got %+v", "[", ctx.Skipped)
	}
}

// TestCollectContextWalkError: an unreadable subdir is recorded as Skipped, not vanished silently.
func TestCollectContextWalkError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permission bits")
	}
	dir := t.TempDir()
	sub := filepath.Join(dir, "locked")
	if err := os.Mkdir(sub, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(sub, 0o755) }) // let t.TempDir() clean up

	ctx, err := CollectContext(dir, []string{"."}, nil, 40, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range ctx.Skipped {
		if s.Path == "locked" && strings.HasPrefix(s.Reason, "walk-error: ") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a walk-error Skipped entry for %q, got %+v", "locked", ctx.Skipped)
	}
}

// TestCollectContextSkipsSecretLike: secret-like paths are always skipped, even outside a git repo.
func TestCollectContextSkipsSecretLike(t *testing.T) {
	dir := t.TempDir()
	w := func(rel, content string) { write(t, filepath.Join(dir, rel), content) }
	w("a.go", "package a")
	w(".env", "SECRET=1")
	w(".env.local", "SECRET=2")
	w("secrets.env", "SECRET=3")
	w("k.pem", "cert")
	w("k.key", "key")
	w("env.d/x.env", "x")
	w("state/y.txt", "y")

	ctx, err := CollectContext(dir, []string{"."}, nil, 40, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(ctx.Included) != 1 || ctx.Included[0].Path != "a.go" {
		t.Errorf("Included = %+v, want only a.go", ctx.Included)
	}
	skipReasons := map[string]string{}
	for _, s := range ctx.Skipped {
		skipReasons[s.Path] = s.Reason
	}
	for _, p := range []string{".env", ".env.local", "secrets.env", "k.pem", "k.key", "env.d/x.env", "state/y.txt"} {
		if skipReasons[p] != "secret-like" {
			t.Errorf("%s reason = %q, want secret-like", p, skipReasons[p])
		}
	}
}

// TestCollectContextHonoursGitIgnore: inside a git work tree, gitignored files are skipped, both
// tracked and untracked files are included.
func TestCollectContextHonoursGitIgnore(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.go"), "package a")
	write(t, filepath.Join(dir, ".gitignore"), "ignored.txt\n.gitignore\n")
	write(t, filepath.Join(dir, "u.go"), "package u")
	write(t, filepath.Join(dir, "ignored.txt"), "ignore me")

	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
		{"add", "a.go"},
		{"commit", "-m", "base"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	ctx, err := CollectContext(dir, []string{"."}, nil, 40, 100)
	if err != nil {
		t.Fatal(err)
	}
	var included []string
	for _, f := range ctx.Included {
		included = append(included, f.Path)
	}
	sort.Strings(included)
	if !reflect.DeepEqual(included, []string{"a.go", "u.go"}) {
		t.Errorf("Included = %v, want [a.go u.go]", included)
	}
	skipReasons := map[string]string{}
	for _, s := range ctx.Skipped {
		skipReasons[s.Path] = s.Reason
	}
	if skipReasons["ignored.txt"] != "gitignored" {
		t.Errorf("ignored.txt reason = %q, want gitignored", skipReasons["ignored.txt"])
	}
}

// TestCollectContextAbsDir: an absolute --dirs entry resolves independent of cwd.
func TestCollectContextAbsDir(t *testing.T) {
	target := t.TempDir()
	write(t, filepath.Join(target, "a.go"), "package a")

	other := t.TempDir()
	ctx, err := CollectContext(other, []string{target}, nil, 40, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range ctx.Included {
		if strings.HasSuffix(f.Path, "a.go") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a.go under absolute dir %s to be found, got %+v", target, ctx.Included)
	}
}

// TestCollectContextBoundedRead: a large file is bounded-read: original size preserved, content capped.
func TestCollectContextBoundedRead(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "big.txt"), strings.Repeat("x", 1<<20))

	ctx, err := CollectContext(dir, []string{"."}, nil, 40, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if len(ctx.Included) != 1 {
		t.Fatalf("Included = %d, want 1", len(ctx.Included))
	}
	f := ctx.Included[0]
	if f.Bytes != 1<<20 {
		t.Errorf("Bytes = %d, want %d", f.Bytes, 1<<20)
	}
	if !f.Truncated {
		t.Error("expected Truncated = true")
	}
	if len(f.Content) > 1024 {
		t.Errorf("len(Content) = %d, want <= 1024", len(f.Content))
	}
}

// TestBuildPrompt: template contains inventory, file blocks, task, and constraints.
func TestBuildPrompt(t *testing.T) {
	ctx := Context{
		Included: []IncludedFile{{Path: "a.go", MediaType: "text/plain", Bytes: 8, Truncated: false, Content: "package a"}},
		Skipped:  []SkippedFile{{Path: "x.png", Reason: "unsupported-extension"}},
	}
	got := BuildPrompt("do a thing", ctx)
	for _, want := range []string{
		"<context_inventory>",
		"- a.go | text/plain | 8 bytes | truncated=false",
		"Skipped files:",
		"- x.png (unsupported-extension)",
		`<file path="a.go" media_type="text/plain" truncated="false">package a</file>`,
		"<task>\ndo a thing\n</task>",
		"Do not invent files",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q:\n%s", want, got)
		}
	}
}

// TestBuildPromptEmpty: no files → "Included files: none" + no-payload sentinel.
func TestBuildPromptEmpty(t *testing.T) {
	got := BuildPrompt("task", Context{})
	if !strings.Contains(got, "Included files: none") {
		t.Error("expected 'Included files: none'")
	}
	if !strings.Contains(got, "No inline file payloads were collected.") {
		t.Error("expected no-payload sentinel")
	}
}
