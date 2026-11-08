package vaultcmd

import (
	"bytes"
	"encoding/base64"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/vault"
)

// assertGetKeyRefusesTTY runs `vault get --key` with slave (a pty slave) as stdout and expects the refusal.
func assertGetKeyRefusesTTY(t *testing.T, slave *os.File) {
	t.Helper()
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int {
		old := os.Stdout
		os.Stdout = slave
		defer func() { os.Stdout = old }()
		return Run([]string{"get", "--key"})
	})
	if code != 2 || stdout != "" || !strings.Contains(stderr, "refusing") {
		t.Fatalf("get --key on TTY: exit %d, stdout empty=%v, refusal=%v",
			code, stdout == "", strings.Contains(stderr, "refusing"))
	}
}

func TestCmdVaultGetKeyNonTTYDecodesToMasterKey(t *testing.T) {
	vaulttest.UseFreshKey(t)
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return Run([]string{"get", "--key"}) })
	if code != 0 {
		t.Fatalf("vault get --key = %d, want 0; stderr empty=%v", code, stderr == "")
	}
	got, err := base64.StdEncoding.DecodeString(strings.TrimSpace(stdout))
	if err != nil {
		t.Fatal("vault get --key stdout does not decode as base64")
	}
	if len(got) != 32 {
		t.Errorf("decoded master key is %d bytes, want 32", len(got))
	}
	want, err := base64.StdEncoding.DecodeString(os.Getenv("HARNESS_VAULT_KEY"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Error("vault get --key printed a different key than HARNESS_VAULT_KEY")
	}
}

func TestVaultDeleteStoredSecret(t *testing.T) {
	vaulttest.UseFreshKey(t)
	const value = "obviously-fake-delete"
	if err := vault.Store("VAULT_TEST_DUMMY", value); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int {
		return Run([]string{"delete", "VAULT_TEST_DUMMY"})
	})
	if code != 0 || len(vault.Names()) != 0 {
		t.Fatalf("delete: exit %d, stored names=%v", code, vault.Names())
	}
	if strings.Contains(stdout+stderr, value) {
		t.Fatal("delete leaked the raw value")
	}
}

func TestVaultExportRejectsStdout(t *testing.T) {
	vaulttest.UseFreshKey(t)
	if err := vault.Store("ALPHA", "obviously-fake-alpha"); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return Run([]string{"export", "--out", "-"}) })
	if code != 2 || stdout != "" || !strings.Contains(stderr, "vault get") {
		t.Fatalf("export --out -: exit %d, stdout empty=%v, get hint=%v",
			code, stdout == "", strings.Contains(stderr, "vault get"))
	}
}

func TestVaultExportFileOnly(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "out"}[explicit], func(t *testing.T) {
			vaulttest.UseFreshKey(t)
			dir := t.TempDir()
			t.Chdir(dir)
			const value = "obviously-fake-alpha"
			if err := vault.Store("ALPHA", value); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, ".env")
			args := []string{"export"}
			if explicit {
				path = filepath.Join(dir, "export.env")
				args = append(args, "--out", path)
			}
			for range 2 {
				code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return Run(args) })
				if code != 0 || stdout != "" || strings.Contains(stderr, value) {
					t.Fatalf("export: exit %d, stdout empty=%v, leaked=%v",
						code, stdout == "", strings.Contains(stderr, value))
				}
			}
			checkExportedFile(t, path, value)
		})
	}
}

// checkExportedFile asserts path is 0600 and holds exactly one ALPHA row whose masked value matches value's.
func checkExportedFile(t *testing.T, path, value string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("export permissions = %o, want 600", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pairs, problems := vault.ParseDotenv(strings.NewReader(string(data)))
	if len(problems) != 0 || len(pairs) != 1 || pairs[0].Name != "ALPHA" {
		t.Fatal("export did not preserve exactly one masked NAME=value row")
	}
	if vault.Redact(pairs[0].Value) != vault.Redact(value) {
		t.Fatal("export changed the masked value")
	}
}

func TestVaultImportDashFromPipeRequiresYes(t *testing.T) {
	vaulttest.UseFreshKey(t)
	withStdin(t, "GAMMA=obviously-fake-gamma\n")
	code, _, stderr := hosttest.CaptureOutput(t, func() int { return Run([]string{"import", "-"}) })
	if code == 0 || !strings.Contains(stderr, "--yes") {
		t.Fatalf("import - without --yes: exit %d, missing confirmation hint=%v",
			code, !strings.Contains(stderr, "--yes"))
	}
	if v, _, _ := vault.Lookup("GAMMA"); v != "" {
		t.Fatal("GAMMA stored despite missing --yes")
	}
}

func TestVaultImportDashFromPipeWithYesStores(t *testing.T) {
	vaulttest.UseFreshKey(t)
	values := map[string]string{"GAMMA": "obviously-fake-gamma", "DELTA": "obviously-fake-delta"}
	withStdin(t, "GAMMA="+values["GAMMA"]+"\nDELTA="+values["DELTA"]+"\n")
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return Run([]string{"import", "-", "--yes"}) })
	if code != 0 {
		t.Fatalf("import - --yes exit = %d", code)
	}
	for name, value := range values {
		got, source, err := vault.Lookup(name)
		if err != nil || source != "vault" || vault.Redact(got) != vault.Redact(value) {
			t.Errorf("%s was not imported into the vault correctly", name)
		}
		if strings.Contains(stdout+stderr, value) {
			t.Errorf("import leaked %s", name)
		}
	}
}

func TestVaultImportDashMalformedWritesNothing(t *testing.T) {
	vaulttest.UseFreshKey(t)
	withStdin(t, "GAMMA=obviously-fake-gamma\nnot-a-pair\n")
	code, _, stderr := hosttest.CaptureOutput(t, func() int { return Run([]string{"import", "-", "--yes"}) })
	if code == 0 || !strings.Contains(stderr, "nothing written") {
		t.Fatalf("malformed import: exit %d, nothing-written hint=%v",
			code, strings.Contains(stderr, "nothing written"))
	}
	if v, _, _ := vault.Lookup("GAMMA"); v != "" {
		t.Fatal("GAMMA stored despite a malformed sibling line")
	}
}

func TestVaultGetMultipleNames(t *testing.T) {
	for _, export := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "export"}[export], func(t *testing.T) {
			vaulttest.UseFreshKey(t)
			values := []string{"obviously-fake-alpha", "obviously-fake-beta's-value"}
			for i, name := range []string{"ALPHA", "BETA"} {
				if err := vault.Store(name, values[i]); err != nil {
					t.Fatal(err)
				}
			}
			args := []string{"get", "ALPHA", "BETA"}
			if export {
				args = append(args, "--export")
			}
			code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return Run(args) })
			if code != 0 || stderr != "" {
				t.Fatalf("get: exit %d, stderr empty=%v", code, stderr == "")
			}
			if got, want := maskGetOutput(stdout, []string{"ALPHA", "BETA"}, values, export); got != want {
				t.Fatal("get did not produce the expected masked NAME=value rows")
			}
		})
	}
}

// maskGetOutput redacts each value in stdout (shell-quoted with export) and builds the masked rows get should print.
func maskGetOutput(stdout string, names, values []string, export bool) (got, want string) {
	var wantSb strings.Builder
	for i, name := range names {
		value := values[i]
		if export {
			value = textutil.ShellQuote(value)
		}
		stdout = strings.ReplaceAll(stdout, value, vault.Redact(values[i]))
		prefix := ""
		if export {
			prefix = "export "
		}
		wantSb.WriteString(prefix)
		wantSb.WriteString(name)
		wantSb.WriteByte('=')
		wantSb.WriteString(vault.Redact(values[i]))
		wantSb.WriteByte('\n')
	}
	return stdout, wantSb.String()
}

func TestVaultGetMissingNamePrintsNothing(t *testing.T) {
	vaulttest.UseFreshKey(t)
	t.Setenv("ABSENT_VAULT_TEST_KEY", "")
	if err := vault.Store("ALPHA", "obviously-fake-alpha"); err != nil {
		t.Fatal(err)
	}
	code, stdout, _ := hosttest.CaptureOutput(t, func() int {
		return Run([]string{"get", "ALPHA", "ABSENT_VAULT_TEST_KEY"})
	})
	if code != 1 || stdout != "" {
		t.Fatalf("missing get: exit %d, stdout empty=%v", code, stdout == "")
	}
}

func TestVaultGetKeyRejectsNamesAndExport(t *testing.T) {
	vaulttest.UseFreshKey(t)
	for _, args := range [][]string{
		{"get", "--key", "ALPHA"},
		{"get", "--key", "--export"},
	} {
		code, stdout, _ := hosttest.CaptureOutput(t, func() int { return Run(args) })
		if code != 2 || stdout != "" {
			t.Errorf("get --key invalid combination: exit %d, stdout empty=%v", code, stdout == "")
		}
	}
}

func TestVaultImportRejectsBadGitHubToken(t *testing.T) {
	vaulttest.UseFreshKey(t)
	withStdin(t, "GITHUB_WORK_PAT=not-a-token\n")
	code, _, stderr := hosttest.CaptureOutput(t, func() int { return Run([]string{"import", "-", "--yes"}) })
	if code == 0 || !strings.Contains(stderr, "not a GitHub token") {
		t.Fatalf("exit %d, stderr=%q, want a GitHub-token rejection", code, stderr)
	}
	if v, _, _ := vault.Lookup("GITHUB_WORK_PAT"); v != "" {
		t.Fatal("bad GitHub token stored by import")
	}
}
