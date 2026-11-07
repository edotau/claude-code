package completion

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScriptRegistersOnlyBinary(t *testing.T) {
	for _, bin := range []string{"claude-code", "custom-harness"} {
		t.Run(bin, func(t *testing.T) {
			script := Script(bin)
			var registrations []string
			for line := range strings.SplitSeq(script, "\n") {
				if strings.HasPrefix(line, "complete ") {
					registrations = append(registrations, line)
				}
			}
			want := "complete -o default -F _" + strings.ReplaceAll(bin, "-", "_") + " " + bin
			if len(registrations) != 1 || registrations[0] != want {
				t.Errorf("registrations = %q, want exactly [%q]", registrations, want)
			}
			if strings.Contains(script, "_verb") {
				t.Errorf("script retained a vendor completion function:\n%s", script)
			}
			if !strings.Contains(script, `"${COMP_WORDS[0]}" __complete "${COMP_WORDS[@]:1:COMP_CWORD}"`) {
				t.Errorf("script missing harness completion invocation:\n%s", script)
			}
		})
	}
}

func TestInstallWritesScriptOnlyForBinary(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BASH_COMPLETION_USER_DIR", dir)
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	path, err := Install("claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "completions", "claude-code"); path != want {
		t.Errorf("installed path = %q, want %q", path, want)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != Script("claude-code") {
		t.Errorf("installed script = %q, want Script(claude-code)", body)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "claude-code" {
		t.Errorf("completion directory entries = %v, want only claude-code", entries)
	}
}

func TestInstallReplacesScriptAtomically(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BASH_COMPLETION_USER_DIR", dir)
	path := filepath.Join(dir, "completions", "claude-code")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("old script"), 0o644); err != nil {
		t.Fatal(err)
	}
	held := filepath.Join(dir, "held")
	if err := os.Link(path, held); err != nil {
		t.Skipf("hard links unsupported: %v", err)
	}
	if _, err := Install("claude-code"); err != nil {
		t.Fatal(err)
	}
	if body, _ := os.ReadFile(held); string(body) != "old script" {
		t.Errorf("old inode was rewritten in place: %q, want it untouched (temp file + rename)", body)
	}
	if body, _ := os.ReadFile(path); string(body) != Script("claude-code") {
		t.Errorf("installed script = %q, want Script(claude-code)", body)
	}
}
