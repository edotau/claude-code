package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isolate points HOME and CLAUDE_CONFIG_DIR at fresh temp dirs so a run never touches the real harness.
func isolate(t *testing.T) (home, claude string) {
	t.Helper()
	home = t.TempDir()
	claude = filepath.Join(home, ".claude")
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", claude)
	t.Setenv("XDG_CONFIG_HOME", "")
	return home, claude
}

// writeFile creates parents and writes content at 0o644.
func projectWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// sourcePath is where a source lives: agents/<dir>.md (flat) or skills/<dir>/SKILL.md.
func sourcePath(claude, kind, dir string) string {
	if kind == "agents" {
		return filepath.Join(claude, kind, dir+".md")
	}
	return filepath.Join(claude, kind, dir, "SKILL.md")
}

// writeSource writes the agent or skill source at sourcePath declaring `name`.
func writeSource(t *testing.T, claude, kind, dir, name string) {
	t.Helper()
	projectWriteFile(t, sourcePath(claude, kind, dir), "---\nname: "+name+"\ndescription: test source.\n---\n\nbody\n")
}

func assertFileContains(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !strings.Contains(string(data), want) {
		t.Errorf("%s does not contain %q", path, want)
	}
}

// A "## Memory" heading inside a fenced code block is example text, not a real section, and must survive.
func TestStripMemorySectionInsideFenceNotStripped(t *testing.T) {
	body := "Intro.\n\n```\n## Memory\nexample content, not a real section\n```\n\nTrailer.\n"
	got := stripMemorySection(body)
	if got != body {
		t.Errorf("stripMemorySection stripped a fenced-code '## Memory' line:\ngot:  %q\nwant: %q (unchanged)", got, body)
	}
}
