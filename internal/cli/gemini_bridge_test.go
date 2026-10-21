package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCmdGeminiBridgePrintCommandListsDirFilesAndNeverRunsAgy: --print-command on the default agy
// backend must log the resolved request (including the ingested dir's files) and never actually
// invoke agy — proven by an agy stub on PATH whose marker is checked to never have been touched.
func TestCmdGeminiBridgePrintCommandListsDirFilesAndNeverRunsAgy(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "example.go"), []byte("package x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pathDir := t.TempDir()
	marker := filepath.Join(pathDir, "marker")
	if err := os.WriteFile(filepath.Join(pathDir, "agy"), []byte("#!/bin/sh\ntouch "+shQuote(marker)+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", pathDir)

	code, _, stderr := captureOutput(t, func() int {
		return cmdGeminiBridge([]string{"t", "--dirs", dir, "--print-command"})
	})
	if code != 0 {
		t.Fatalf("--print-command exit %d, want 0; stderr=%q", code, stderr)
	}
	if !strings.Contains(stderr, "example.go") {
		t.Errorf("--print-command output missing the ingested file:\n%s", stderr)
	}
	if !strings.Contains(stderr, "agy ") {
		t.Errorf("--print-command output missing the resolved agy argv:\n%s", stderr)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("marker exists — agy was actually launched, stat err=%v", err)
	}
}

// TestCmdGeminiBridgeTaskAndPositionalAreMutuallyExclusive: --task plus trailing positional args is a
// usage error (2), not silently ignoring one or the other.
func TestCmdGeminiBridgeTaskAndPositionalAreMutuallyExclusive(t *testing.T) {
	code, _, stderr := captureOutput(t, func() int {
		return cmdGeminiBridge([]string{"--task", "x", "y"})
	})
	if code != 2 {
		t.Fatalf("--task x y: exit %d, want 2; stderr=%q", code, stderr)
	}
	if !strings.Contains(stderr, "mutually exclusive") {
		t.Errorf("stderr = %q, want it to mention mutually exclusive", stderr)
	}
}
