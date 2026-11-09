package vaultcmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/ui/tui"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestVaultImportFileYesSkipsTerminalConfirm(t *testing.T) {
	vaulttest.UseFreshKey(t)
	path := filepath.Join(t.TempDir(), "import.txt")
	if err := os.WriteFile(path, []byte("REGRESSION_IMPORT=synthetic-import-value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	master, slave := vaultTestPTY(t)
	old := os.Stdin
	os.Stdin = slave
	t.Cleanup(func() { os.Stdin = old })
	if !tui.StdinIsTerminal() {
		t.Fatal("import regression needs terminal stdin")
	}
	if _, err := master.WriteString("n\n"); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int {
		return Run([]string{"import", path, "--yes"})
	})
	if code != 0 || strings.Contains(stdout+stderr, "Store 1 secret") {
		t.Fatalf("import FILE --yes prompted or failed: exit %d, stdout=%q, stderr=%q", code, stdout, stderr)
	}
	got, found, err := vault.ReadSecret("REGRESSION_IMPORT")
	if err != nil || !found || got != "synthetic-import-value" {
		t.Fatal("import FILE --yes did not store the secret")
	}
}

func TestVaultGetFailsOnFullStdout(t *testing.T) {
	for _, export := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "export"}[export], func(t *testing.T) {
			vaulttest.UseFreshKey(t)
			vaulttest.Secret(t, "REGRESSION_GET", "synthetic-get-value")
			full, err := os.OpenFile("/dev/full", os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { full.Close() })
			args := []string{"get", "REGRESSION_GET"}
			if export {
				args = append(args, "--export")
			}
			code, _, stderr := hosttest.CaptureOutput(t, func() int {
				old := os.Stdout
				os.Stdout = full
				defer func() { os.Stdout = old }()
				return Run(args)
			})
			if code != 1 || !strings.Contains(stderr, "vault get: write /dev/full:") {
				t.Fatalf("get with full stdout: exit %d, stderr=%q", code, stderr)
			}
		})
	}
}
