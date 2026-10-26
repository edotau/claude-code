package helptext

import (
	"os"
	"path/filepath"
	"testing"
)

// WrappedDocs runs `<bin> --help` from PATH and passes its exit code through; a missing binary is exit 1.
func TestWrappedDocsRunsBinaryHelp(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "argv")
	script := "#!/bin/sh\necho \"$@\" > " + out + "\nexit 3\n"
	if err := os.WriteFile(filepath.Join(dir, "fakecli"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	if code := WrappedDocs("fakecli"); code != 3 {
		t.Fatalf("exit = %d, want the binary's 3", code)
	}
	if got, _ := os.ReadFile(out); string(got) != "--help\n" {
		t.Fatalf("argv = %q, want --help", got)
	}
	if code := WrappedDocs("no-such-cli"); code != 1 {
		t.Fatalf("missing binary exit = %d, want 1", code)
	}
}
