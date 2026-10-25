package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/secrets"
)

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
	t.Cleanup(func() {
		os.Stdin = old
		r.Close()
	})
}

func TestVaultExportRejectsStdout(t *testing.T) {
	useVaultTest(t)
	if err := secrets.Store("ALPHA", "obviously-fake-alpha"); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := captureOutput(t, func() int { return cmdVault([]string{"export", "--out", "-"}) })
	if code != 2 || stdout != "" || !strings.Contains(stderr, "vault get") {
		t.Fatalf("export --out -: exit %d, stdout empty=%v, get hint=%v",
			code, stdout == "", strings.Contains(stderr, "vault get"))
	}
}

func TestVaultExportFileOnly(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "out"}[explicit], func(t *testing.T) {
			useVaultTest(t)
			dir := t.TempDir()
			t.Chdir(dir)
			const value = "obviously-fake-alpha"
			if err := secrets.Store("ALPHA", value); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, ".env")
			args := []string{"export"}
			if explicit {
				path = filepath.Join(dir, "export.env")
				args = append(args, "--out", path)
			}
			for range 2 {
				code, stdout, stderr := captureOutput(t, func() int { return cmdVault(args) })
				if code != 0 || stdout != "" || strings.Contains(stderr, value) {
					t.Fatalf("export: exit %d, stdout empty=%v, leaked=%v",
						code, stdout == "", strings.Contains(stderr, value))
				}
			}
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
			pairs, problems := secrets.ParseDotenv(strings.NewReader(string(data)))
			if len(problems) != 0 || len(pairs) != 1 || pairs[0].Name != "ALPHA" {
				t.Fatal("export did not preserve exactly one masked NAME=value row")
			}
			if secrets.Redact(pairs[0].Value) != secrets.Redact(value) {
				t.Fatal("export changed the masked value")
			}
		})
	}
}

func TestVaultImportDashFromPipeRequiresYes(t *testing.T) {
	useVaultTest(t)
	withStdin(t, "GAMMA=obviously-fake-gamma\n")
	code, _, stderr := captureOutput(t, func() int { return cmdVault([]string{"import", "-"}) })
	if code == 0 || !strings.Contains(stderr, "--yes") {
		t.Fatalf("import - without --yes: exit %d, missing confirmation hint=%v",
			code, !strings.Contains(stderr, "--yes"))
	}
	if v, _, _ := secrets.Lookup("GAMMA"); v != "" {
		t.Fatal("GAMMA stored despite missing --yes")
	}
}

func TestVaultImportDashFromPipeWithYesStores(t *testing.T) {
	useVaultTest(t)
	values := map[string]string{"GAMMA": "obviously-fake-gamma", "DELTA": "obviously-fake-delta"}
	withStdin(t, "GAMMA="+values["GAMMA"]+"\nDELTA="+values["DELTA"]+"\n")
	code, stdout, stderr := captureOutput(t, func() int { return cmdVault([]string{"import", "-", "--yes"}) })
	if code != 0 {
		t.Fatalf("import - --yes exit = %d", code)
	}
	for name, value := range values {
		got, source, err := secrets.Lookup(name)
		if err != nil || source != "vault" || secrets.Redact(got) != secrets.Redact(value) {
			t.Errorf("%s was not imported into the vault correctly", name)
		}
		if strings.Contains(stdout+stderr, value) {
			t.Errorf("import leaked %s", name)
		}
	}
}

func TestVaultImportDashMalformedWritesNothing(t *testing.T) {
	useVaultTest(t)
	withStdin(t, "GAMMA=obviously-fake-gamma\nnot-a-pair\n")
	code, _, stderr := captureOutput(t, func() int { return cmdVault([]string{"import", "-", "--yes"}) })
	if code == 0 || !strings.Contains(stderr, "nothing written") {
		t.Fatalf("malformed import: exit %d, nothing-written hint=%v",
			code, strings.Contains(stderr, "nothing written"))
	}
	if v, _, _ := secrets.Lookup("GAMMA"); v != "" {
		t.Fatal("GAMMA stored despite a malformed sibling line")
	}
}

func TestVaultGetMultipleNames(t *testing.T) {
	for _, export := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "export"}[export], func(t *testing.T) {
			useVaultTest(t)
			values := []string{"obviously-fake-alpha", "obviously-fake-beta's-value"}
			for i, name := range []string{"ALPHA", "BETA"} {
				if err := secrets.Store(name, values[i]); err != nil {
					t.Fatal(err)
				}
			}
			args := []string{"get", "ALPHA", "BETA"}
			if export {
				args = append(args, "--export")
			}
			code, stdout, stderr := captureOutput(t, func() int { return cmdVault(args) })
			if code != 0 || stderr != "" {
				t.Fatalf("get: exit %d, stderr empty=%v", code, stderr == "")
			}
			want := ""
			var wantSb151 strings.Builder
			for i, name := range []string{"ALPHA", "BETA"} {
				value := values[i]
				if export {
					value = "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
				}
				stdout = strings.ReplaceAll(stdout, value, secrets.Redact(values[i]))
				prefix := ""
				if export {
					prefix = "export "
				}
				wantSb151.WriteString(prefix + name + "=" + secrets.Redact(values[i]) + "\n")
			}
			want += wantSb151.String()
			if stdout != want {
				t.Fatal("get did not produce the expected masked NAME=value rows")
			}
		})
	}
}

func TestVaultGetMissingNamePrintsNothing(t *testing.T) {
	useVaultTest(t)
	t.Setenv("ABSENT_VAULT_TEST_KEY", "")
	if err := secrets.Store("ALPHA", "obviously-fake-alpha"); err != nil {
		t.Fatal(err)
	}
	code, stdout, _ := captureOutput(t, func() int {
		return cmdVault([]string{"get", "ALPHA", "ABSENT_VAULT_TEST_KEY"})
	})
	if code != 1 || stdout != "" {
		t.Fatalf("missing get: exit %d, stdout empty=%v", code, stdout == "")
	}
}

func TestVaultGetKeyRejectsNamesAndExport(t *testing.T) {
	useVaultTest(t)
	for _, args := range [][]string{
		{"get", "--key", "ALPHA"},
		{"get", "--key", "--export"},
	} {
		code, stdout, _ := captureOutput(t, func() int { return cmdVault(args) })
		if code != 2 || stdout != "" {
			t.Errorf("get --key invalid combination: exit %d, stdout empty=%v", code, stdout == "")
		}
	}
}
