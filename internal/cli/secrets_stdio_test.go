package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// withStdin redirects os.Stdin to a pipe fed with content for the duration of the test.
func withStdin(t *testing.T, content string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString(content); err != nil {
		t.Fatal(err)
	}
	w.Close()
	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = old })
}

func TestSecretsExportOutDashStreamsToStdout(t *testing.T) {
	dir := vaulttest.Use(t)
	if err := secrets.Store("ALPHA", "alpha-value"); err != nil {
		t.Fatal(err)
	}
	if err := secrets.Store("BETA", "beta-value"); err != nil {
		t.Fatal(err)
	}

	code, stdout, _ := captureOutput(t, func() int { return cmdSecretsExport([]string{"--out", "-"}) })
	if code != 0 {
		t.Fatalf("export --out - exit = %d", code)
	}
	if !strings.Contains(stdout, "ALPHA=alpha-value\n") || !strings.Contains(stdout, "BETA=beta-value\n") {
		t.Fatalf("stdout = %q, want both NAME=value lines", stdout)
	}
	if _, err := os.Stat(dir + "/.env"); !os.IsNotExist(err) {
		t.Fatalf(".env under CLAUDE_CONFIG_DIR: stat err = %v, want not-exist", err)
	}
	if _, err := os.Stat(".env"); !os.IsNotExist(err) {
		t.Fatalf("./.env created by --out -: stat err = %v, want not-exist", err)
	}
}

func TestSecretsImportDashFromPipeRequiresYes(t *testing.T) {
	vaulttest.Use(t)
	withStdin(t, "GAMMA=gamma-value\n")

	code, _, stderr := captureOutput(t, func() int { return cmdSecretsImport([]string{"-"}) })
	if code == 0 {
		t.Fatalf("import - without --yes exit = 0, want nonzero")
	}
	if !strings.Contains(stderr, "--yes") {
		t.Fatalf("stderr = %q, want a --yes mention", stderr)
	}
	if v, _, _ := secrets.Lookup("GAMMA"); v != "" {
		t.Fatal("GAMMA stored despite missing --yes")
	}
}

func TestSecretsImportDashFromPipeWithYesStores(t *testing.T) {
	vaulttest.Use(t)
	withStdin(t, "GAMMA=gamma-value\nDELTA=delta-value\n")

	code, _, _ := captureOutput(t, func() int { return cmdSecretsImport([]string{"-", "--yes"}) })
	if code != 0 {
		t.Fatalf("import - --yes exit = %d", code)
	}
	if v, _, err := secrets.Lookup("GAMMA"); err != nil || v != "gamma-value" {
		t.Fatalf("GAMMA = %q, %v, want gamma-value, nil", v, err)
	}
	if v, _, err := secrets.Lookup("DELTA"); err != nil || v != "delta-value" {
		t.Fatalf("DELTA = %q, %v, want delta-value, nil", v, err)
	}
}

func TestSecretsImportDashMalformedWritesNothing(t *testing.T) {
	vaulttest.Use(t)
	withStdin(t, "GAMMA=gamma-value\nnot-a-pair\n")

	code, _, stderr := captureOutput(t, func() int { return cmdSecretsImport([]string{"-", "--yes"}) })
	if code == 0 {
		t.Fatal("import - with a malformed line exit = 0, want nonzero")
	}
	if !strings.Contains(stderr, "nothing written") {
		t.Fatalf("stderr = %q, want nothing-written", stderr)
	}
	if v, _, _ := secrets.Lookup("GAMMA"); v != "" {
		t.Fatal("GAMMA stored despite a malformed sibling line")
	}
}

// TestSecretsImportDashRefusesOnTerminal documents the TTY refusal; it cannot drive isTerminal(fd) to
// true from a test (no seam over a real controlling terminal), so it only pins the non-TTY paths above.
func TestSecretsImportDashRefusesOnTerminal(t *testing.T) {
	t.Skip("no seam to simulate a controlling terminal on stdin from within go test; verified manually")
}
