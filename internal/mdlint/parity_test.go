package mdlint

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeMdsmith is a script at ConfigDir/bin/mdsmith: init prints the expected converter note, resolve a fixed rule.
const fakeMdsmith = `#!/bin/sh
case "$1" in
init) echo 'note: MD010 option "code_blocks": no mdsmith equivalent' >&2 ;;
kinds) echo '{"rules":{"line-length":{"leaves":[{"path":"max","value":120}]}}}' ;;
esac
`

// parityRoot copies the clean fixture into a root with .mdsmith.yml and points mdsmith at the fake.
func parityRoot(t *testing.T) string {
	t.Helper()
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	if err := os.MkdirAll(filepath.Join(cfg, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "bin", "mdsmith"), []byte(fakeMdsmith), 0o700); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	for _, file := range []string{RulesFile, CLI2File} {
		contents, err := os.ReadFile(filepath.Join("testdata", "parity", "clean", file))
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(root, file), contents, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("# Parity\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	yml := "files:\n  - '**/*.md'\nignore:\n  - 'internal/**/testdata/**'\n"
	if err := os.WriteFile(filepath.Join(root, MdsmithFile), []byte(yml), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestParityClean(t *testing.T) {
	lines, err := Parity(context.Background(), parityRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) > 0 {
		t.Fatalf("unexpected drift: %v", lines)
	}
}

func TestParityDrift(t *testing.T) {
	root := parityRoot(t)
	rules := `{"default":true,"MD013":{"line_length":100,"stern":true},"MD010":{"code_blocks":false}}`
	ignores := `{"ignores":["internal/**/testdata/**","extra/**"]}`
	if err := os.WriteFile(filepath.Join(root, RulesFile), []byte(rules), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, CLI2File), []byte(ignores), 0o600); err != nil {
		t.Fatal(err)
	}
	lines, err := Parity(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(lines, "\n")
	if len(lines) != 2 || !strings.Contains(all, "MD013.line_length") || !strings.Contains(all, "ignore:") {
		t.Fatalf("drift = %v", lines)
	}
}
