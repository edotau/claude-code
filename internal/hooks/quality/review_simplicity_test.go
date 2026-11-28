package quality

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/platform/hosttest"
)

const rsClean = "package fx\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n"

func rsRun(t *testing.T, tool, path, oldS, newS string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"hook_event_name": "PostToolUse", "tool_name": tool, "cwd": filepath.Dir(path),
		"tool_input": map[string]any{"file_path": path, "old_string": oldS, "new_string": newS},
	})
	if err != nil {
		t.Fatal(err)
	}
	res := ReviewSimplicity(core.ParseInput(bytes.NewReader(payload)))
	if res.Decision != core.Allow {
		t.Fatalf("decision %d, want Allow", res.Decision)
	}
	return res.Context
}

func rsWrite(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReviewSimplicityEdit(t *testing.T) {
	block := "// first prose line\n// second prose line\n"
	after := rsClean + "\n" + block + "func Z() {}\n"
	p := rsWrite(t, "a.go", after)

	out := rsRun(t, "Edit", p, "", block)
	if !strings.Contains(out, "long_comment") || !strings.Contains(out, "1 new finding") {
		t.Errorf("context = %q, want one long_comment finding", out)
	}

	// Same noisy file, an unrelated edit: the old comment block is not re-reported.
	if out := rsRun(t, "Edit", p, "func Z() {}", "func Z() {}"); out != "" {
		t.Errorf("unchanged findings produced output: %q", out)
	}
}

func TestReviewSimplicityIgnoresNonSource(t *testing.T) {
	p := rsWrite(t, "notes.md", "# t\n\n// a\n// b\n")
	if out := rsRun(t, "Edit", p, "", "// a\n// b\n"); out != "" {
		t.Errorf(".md produced output: %q", out)
	}
}

func TestReviewSimplicityWriteUsesGitHead(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := hosttest.Repo(t)
	noisy := rsClean + "\n// old one\n// old two\nfunc Old() {}\n"
	p := filepath.Join(dir, "a.go")
	if err := os.WriteFile(p, []byte(noisy), 0o644); err != nil {
		t.Fatal(err)
	}
	hosttest.In(t, dir, "add", "a.go")
	hosttest.In(t, dir, "commit", "-q", "-m", "x")
	// Rewrite the whole file with the old block intact: only the added block is new.
	if err := os.WriteFile(p, []byte(noisy+"\n// new one\n// new two\nfunc N() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := rsRun(t, "Write", p, "", "")
	if !strings.Contains(out, "1 new finding") {
		t.Errorf("Write vs HEAD output = %q, want exactly 1 new finding", out)
	}
	// An untracked file has no HEAD: every finding is new.
	q := filepath.Join(dir, "b.go")
	if err := os.WriteFile(q, []byte(noisy), 0o644); err != nil {
		t.Fatal(err)
	}
	if out := rsRun(t, "Write", q, "", ""); !strings.Contains(out, "1 new finding") {
		t.Errorf("untracked Write output = %q, want 1 new finding", out)
	}
}

func TestReviewSimplicityEditDeletionReportsNothing(t *testing.T) {
	noisy := rsClean + "\n// old one\n// old two\nfunc Old() {}\n"
	p := rsWrite(t, "a.go", noisy)
	if out := rsRun(t, "Edit", p, "func Gone() {}\n", ""); out != "" {
		t.Errorf("deletion edit attributed findings to the edit: %q", out)
	}
	in := core.ParseInput(strings.NewReader(`{"tool_name":"Edit","tool_input":{"old_string":"x","new_string":"","replace_all":true}}`))
	if before, ok := editBefore(in, noisy); ok {
		t.Errorf("editBefore ok=true for a deletion, before = %q", before)
	}
}

func TestReviewSimplicityGitFailureReportsNothing(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := hosttest.Repo(t)
	noisy := rsClean + "\n// old one\n// old two\nfunc Old() {}\n"
	p := filepath.Join(dir, "a.go")
	if err := os.WriteFile(p, []byte(noisy), 0o644); err != nil {
		t.Fatal(err)
	}
	hosttest.In(t, dir, "add", "a.go")
	hosttest.In(t, dir, "commit", "-q", "-m", "x")
	// Corrupt the repo: delete the committed blob so `git show HEAD:./a.go` fails for a reason other than untracked.
	s := strings.TrimSpace(hosttest.In(t, dir, "rev-parse", "HEAD:a.go"))
	if err := os.Remove(filepath.Join(dir, ".git", "objects", s[:2], s[2:])); err != nil {
		t.Fatal(err)
	}
	if out := rsRun(t, "Write", p, "", ""); out != "" {
		t.Errorf("git failure output = %q, want no advisory (old debt must not be blamed on the edit)", out)
	}
}

// A Write to a file no source language claims never shells out to git; a Go file still does and reports.
func TestReviewSimplicityUnsupportedSkipsGit(t *testing.T) {
	calls := 0
	orig := gitShowHead
	gitShowHead = func(context.Context, string, ...string) (string, error) { calls++; return "", nil }
	t.Cleanup(func() { gitShowHead = orig })
	noisy := rsClean + "\n// old one\n// old two\nfunc Old() {}\n"
	if out := rsRun(t, "Write", rsWrite(t, "notes.md", noisy), "", ""); out != "" || calls != 0 {
		t.Errorf(".md Write: output %q, git calls %d; want no output and no git call", out, calls)
	}
	if out := rsRun(t, "Write", rsWrite(t, "a.go", noisy), "", ""); !strings.Contains(out, "1 new finding") || calls != 1 {
		t.Errorf(".go Write: output %q, git calls %d; want 1 new finding after 1 git call", out, calls)
	}
}

func TestReviewSimplicityWriteWithoutEnglishGitErrors(t *testing.T) {
	t.Setenv("LC_ALL", "de_DE.UTF-8")
	t.Setenv("LANGUAGE", "de")
	dir := t.TempDir()
	hosttest.In(t, dir, "init", "-q")
	path := filepath.Join(dir, "new.go")
	hosttest.WriteRel(t, dir, "new.go", rsClean+"\n// first\n// second\nfunc N() {}\n")
	if got := rsRun(t, "Write", path, "", ""); !strings.Contains(got, "1 new finding") {
		t.Errorf("unborn HEAD: %q", got)
	}
	hosttest.In(t, dir, "commit", "--allow-empty", "-q", "-m", "initial")
	if got := rsRun(t, "Write", path, "", ""); !strings.Contains(got, "1 new finding") {
		t.Errorf("untracked path with localized git: %q", got)
	}
}

func TestReviewSimplicityMarkdown(t *testing.T) {
	config := fakeMdsmith(t)
	hosttest.WriteExec(t, filepath.Join(config, "bin", "mdsmith"), "#!/bin/sh\nprintf '%s' "+
		"'[{\"file\":\"p.md\",\"line\":1,\"rule\":\"MDS002\",\"message\":\"old\"},"+
		"{\"file\":\"p.md\",\"line\":3,\"rule\":\"MDS001\",\"message\":\"too long\"}]'\n")
	p := filepath.Join(config, "plans", "p.md")
	hosttest.WriteRel(t, config, "plans/p.md", "# Old\n\nnew long line\n")
	out := rsRun(t, "Edit", p, "", "new long line")
	if !strings.Contains(out, "MDS001") || strings.Contains(out, "MDS002") || !strings.Contains(out, "markdown lint") {
		t.Errorf("Edit context = %q, want only the MDS001 row", out)
	}
	if out := rsRun(t, "Edit", p, "short", "long"); !strings.Contains(out, "MDS001") {
		t.Errorf("one-word edit hid its line's finding: %q", out)
	}
	if out := rsRun(t, "Write", p, "", ""); !strings.Contains(out, "MDS002") {
		t.Errorf("Write context = %q, want every finding", out)
	}
	hosttest.WriteRel(t, config, "plans/p.md", "# Old\n\nnew long line\nnew long line\n")
	if out := rsRun(t, "Edit", p, "", "# Old"); strings.Contains(out, "MDS001") {
		t.Errorf("edit of line 1 blamed line 3: %q", out)
	}
	hosttest.WriteRel(t, config, "plans/p.md", "# Old\n\nnew long line\ntail\n")
	if out := rsRun(t, "Edit", p, "# Old\n\nnew short line\ntail", "# Old\n\nnew long line\ntail"); !strings.Contains(out, "MDS001") || strings.Contains(out, "MDS002") {
		t.Errorf("multi-line edit changing line 3 = %q, want only MDS001", out)
	}
	if out := rsRun(t, "Edit", p, "# Old\n\nnew long line\nold tail", "# Old\n\nnew long line\ntail"); out != "" {
		t.Errorf("unchanged lines of a multi-line edit were flagged: %q", out)
	}
	hosttest.WriteRel(t, config, "plans/p.md", "# Old\n\nnew long line\nnew long line\n")
	if out := rsRun(t, "Edit", p, "short", "new long line"); out != "" {
		t.Errorf("repeated new_string blamed a possibly pre-existing line: %q", out)
	}
	if out := rsRun(t, "Edit", p, "", "untouched"); out != "" {
		t.Errorf("unrelated edit produced %q", out)
	}
}

// rsBenchSource is a 600-line Go file of small functions plus one over the complexity limit, so findings exist.
func rsBenchSource() string {
	var b strings.Builder
	b.WriteString("package fx\n\nimport \"strings\"\n\n")
	for i := range 62 {
		fmt.Fprintf(&b, "// F%d trims s and reports whether it holds %d.\nfunc F%d(s string) bool {\n\ts = strings.TrimSpace(s)\n"+
			"\tif s == \"\" {\n\t\treturn false\n\t}\n\treturn strings.Contains(s, \"%d\")\n}\n\n", i, i, i, i)
	}
	b.WriteString("func Busy(a, b, c, d, e, f, g, h, i, j, k int) int {\n\tn := 0\n")
	for _, v := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k"} {
		fmt.Fprintf(&b, "\tif %s > 0 && n < 9 {\n\t\tn++\n\t}\n", v)
	}
	b.WriteString("\treturn n\n}\n")
	return b.String()
}

// BenchmarkReviewSimplicityEdit is one Edit of a 600-line Go file whose findings predate the edit.
func BenchmarkReviewSimplicityEdit(b *testing.B) {
	src := rsBenchSource()
	p := filepath.Join(b.TempDir(), "fx.go")
	edited := strings.Replace(src, "\"7\")", "\"seven\")", 1)
	if err := os.WriteFile(p, []byte(edited), 0o644); err != nil {
		b.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{"hook_event_name": "PostToolUse", "tool_name": "Edit", "cwd": filepath.Dir(p),
		"tool_input": map[string]any{"file_path": p, "old_string": "\"7\")", "new_string": "\"seven\")"}})
	b.Logf("%d lines, context %q", strings.Count(edited, "\n"), ReviewSimplicity(core.ParseInput(bytes.NewReader(payload))).Context)
	b.ReportAllocs()
	for b.Loop() {
		ReviewSimplicity(core.ParseInput(bytes.NewReader(payload)))
	}
}
