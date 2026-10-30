package hooks

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
	var out bytes.Buffer
	if code := runDirect("review-simplicity", bytes.NewReader(payload), &out, nil); code != ExitProceed {
		t.Fatalf("exit %d, want %d", code, ExitProceed)
	}
	return out.String()
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
	var doc struct {
		HookSpecificOutput struct {
			AdditionalContext string `json:"additionalContext"`
			HookEventName     string `json:"hookEventName"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("stdout is not hook JSON: %q (%v)", out, err)
	}
	ctx := doc.HookSpecificOutput.AdditionalContext
	if doc.HookSpecificOutput.HookEventName != "PostToolUse" || !strings.Contains(ctx, "long_comment") ||
		!strings.Contains(ctx, "1 new finding") || !strings.HasSuffix(ctx, simplicityFixHint) {
		t.Errorf("additionalContext = %q (event %q), want one long_comment finding", ctx, doc.HookSpecificOutput.HookEventName)
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
	dir := t.TempDir()
	for _, a := range [][]string{{"init", "-q"}, {"config", "user.email", "t@example.com"}, {"config", "user.name", "t"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, a...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", a, err, out)
		}
	}
	noisy := rsClean + "\n// old one\n// old two\nfunc Old() {}\n"
	p := filepath.Join(dir, "a.go")
	if err := os.WriteFile(p, []byte(noisy), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, a := range [][]string{{"add", "a.go"}, {"commit", "-q", "-m", "x"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, a...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", a, err, out)
		}
	}
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
