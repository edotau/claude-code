package crossgen

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newDocsHarness builds a minimal harness (CLAUDE.md + one agent) under an isolated CLAUDE_CONFIG_DIR.
func newDocsHarness(t *testing.T) string {
	t.Helper()
	_, claude := isolate(t)
	writeFile(t, filepath.Join(claude, "CLAUDE.md"), "# CLAUDE.md\n\nBody.\n\n\n")
	writeFile(t, filepath.Join(claude, "agents", "demo-agent.md"),
		"---\nname: demo-agent\ndescription: demo agent for the docs test. More words.\nmodel: sonnet\n---\n\nBody.\n")
	return claude
}

// Foreign tools read AGENTS.md / GEMINI.md from the root only, and both must be the same bytes.
func TestRenderDocsWritesBothRootFilesByteIdentical(t *testing.T) {
	claude := newDocsHarness(t)
	if code := RenderDocs(DocsOptions{Out: io.Discard}); code != 0 {
		t.Fatalf("RenderDocs exit = %d, want 0", code)
	}
	var contents [][]byte
	for _, name := range outputFiles {
		data, err := os.ReadFile(filepath.Join(claude, name))
		if err != nil {
			t.Fatalf("%s missing from the harness root: %v", name, err)
		}
		contents = append(contents, data)
	}
	if !bytes.Equal(contents[0], contents[1]) {
		t.Error("AGENTS.md and GEMINI.md differ")
	}
	got := string(contents[0])
	want := DocsBanner + "\n\n# CLAUDE.md\n\nBody.\n\n## Agents\n\n- **demo-agent** (`sonnet`) — demo agent for the docs test.\n"
	if got != want {
		t.Errorf("render mismatch\n got: %q\nwant: %q", got, want)
	}
}

// --check must gate on the root copies, or a hand-deleted/stale AGENTS.md passes CI silently.
func TestRenderDocsCheckGatesRootCopy(t *testing.T) {
	claude := newDocsHarness(t)
	if code := RenderDocs(DocsOptions{Out: io.Discard}); code != 0 {
		t.Fatalf("RenderDocs exit = %d, want 0", code)
	}
	if code := RenderDocs(DocsOptions{Check: true, Out: io.Discard}); code != 0 {
		t.Fatalf("--check on a fresh render = %d, want 0", code)
	}
	if err := os.Remove(filepath.Join(claude, outputFiles[0])); err != nil {
		t.Fatal(err)
	}
	if code := RenderDocs(DocsOptions{Check: true, Out: io.Discard}); code == 0 {
		t.Errorf("--check passed with %s deleted; drift went undetected", outputFiles[0])
	}
	if _, err := os.Stat(filepath.Join(claude, outputFiles[0])); err == nil {
		t.Error("--check must not write")
	}
}

// An unchanged re-render must go through the no-op path: identical content, untouched mtime.
func TestRenderDocsSkipsRewriteWhenUnchanged(t *testing.T) {
	claude := newDocsHarness(t)
	if code := RenderDocs(DocsOptions{Out: io.Discard}); code != 0 {
		t.Fatalf("RenderDocs exit = %d, want 0", code)
	}
	p := filepath.Join(claude, outputFiles[0])
	before, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if code := RenderDocs(DocsOptions{Out: io.Discard}); code != 0 {
		t.Fatalf("second RenderDocs exit = %d, want 0", code)
	}
	after, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("%s was rewritten with identical content", outputFiles[0])
	}
}

// The render must never target CLAUDE.md itself — that would overwrite the source with its own derivative.
func TestRenderDocsNeverTargetsConventionsSource(t *testing.T) {
	for _, name := range outputFiles {
		if strings.EqualFold(name, "CLAUDE.md") {
			t.Fatalf("outputFiles contains %s — the render would clobber its own source", name)
		}
	}
}
