package generate

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestRevertLegacySidecarWhenMirrorEmptyStd11 pins: with no mirror backup, revert restores from a legacy <path>.backup.<stamp>.
func TestRevertLegacySidecarWhenMirrorEmptyStd11(t *testing.T) {
	path, _ := std6Env(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".backup.20260101T000000", []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := revertOne("gemini-settings", path, false, &out); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "old" {
		t.Fatalf("config = %q, want %q (output: %s)", got, "old", out.String())
	}
}
