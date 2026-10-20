package launch

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// seedEnvD writes one legacy env.d file under the hermetic config dir.
func seedEnvD(t *testing.T, name, body string) string {
	t.Helper()
	dir := filepath.Join(paths.ConfigDir(), "env.d")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func fakeSelf(t *testing.T) {
	t.Helper()
	self := filepath.Join(t.TempDir(), "claude-code")
	if err := os.WriteFile(self, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	selfBinary = func() (string, error) { return self, nil }
}

func TestInstallMigrationFailureKeepsOldBinary(t *testing.T) {
	hermetic(t, "")
	t.Setenv("HARNESS_VAULT_KEY", "")
	vaulttest.Keyring(t)
	fakeSelf(t)
	src := seedEnvD(t, "secrets.env", "GOOD_KEY=v\nno-equals-sign\n")
	var out bytes.Buffer
	err := Install(&out, false)
	if err == nil || !strings.Contains(err.Error(), "migration incomplete") {
		t.Fatalf("Install err = %v, want migration incomplete\n%s", err, &out)
	}
	if _, err := os.Lstat(filepath.Join(paths.BinDir(), "claude-code")); err == nil {
		t.Error("binary installed despite a failed migration")
	}
	if _, err := os.Stat(src); err != nil {
		t.Errorf("failed source was renamed: %v", err)
	}
}

func TestInstallDryRunReportsMigration(t *testing.T) {
	hermetic(t, "")
	t.Setenv("HARNESS_VAULT_KEY", "")
	vaulttest.Keyring(t)
	fakeSelf(t)
	src := seedEnvD(t, "provider.env", "HARNESS_PROVIDER=openai\n")
	var out bytes.Buffer
	if err := Install(&out, true); err != nil {
		t.Fatalf("Install --dry-run: %v\n%s", err, &out)
	}
	if !strings.Contains(out.String(), "would migrate 1 file(s)") {
		t.Errorf("dry-run output lacks would migrate:\n%s", &out)
	}
	if _, err := os.Stat(src); err != nil {
		t.Errorf("dry-run touched %s: %v", src, err)
	}
	if _, err := os.Stat(vault.Path()); err == nil {
		t.Error("dry-run created the vault")
	}
}

func TestVaultDoctorChecks(t *testing.T) {
	t.Run("missing vault", func(t *testing.T) {
		hermetic(t, "")
		checks := vaultDoctorChecks()
		c := findCheck(checks, "vault")
		if c == nil || c.Detail != "not created yet" || !c.OK {
			t.Errorf("vault row = %+v", c)
		}
		if len(checks) != 1 {
			t.Errorf("want only the vault row, got %+v", checks)
		}
	})
	t.Run("legacy file", func(t *testing.T) {
		hermetic(t, "")
		seedEnvD(t, "provider.env", "HARNESS_PROVIDER=openai\n")
		c := findCheck(vaultDoctorChecks(), "legacy env.d")
		if c == nil || c.OK || !strings.Contains(c.Detail, "1 file(s); run claude-code vault migrate") {
			t.Errorf("legacy row = %+v", c)
		}
	})
	t.Run("manifest", func(t *testing.T) {
		hermetic(t, "")
		state := filepath.Join(paths.ConfigDir(), "state")
		if err := os.MkdirAll(state, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(state, "vault-migration.json"), []byte(`{"items":[]}`), 0o600); err != nil {
			t.Fatal(err)
		}
		c := findCheck(vaultDoctorChecks(), "migration backups")
		if c == nil || c.OK || !strings.Contains(c.Detail, "vault finalize when verified") {
			t.Errorf("backups row = %+v", c)
		}
	})
	t.Run("secrets and retired", func(t *testing.T) {
		hermetic(t, "")
		t.Setenv("HARNESS_VAULT_KEY", "")
		vaulttest.Keyring(t)
		vaulttest.Secret(t, "FOO_API_KEY", "sk-test-0123456789")
		if _, _, err := vault.RotateKey(); err != nil {
			t.Fatal(err)
		}
		c := findCheck(vaultDoctorChecks(), "vault")
		if c == nil || !c.OK {
			t.Fatalf("vault row = %+v", c)
		}
		for _, want := range []string{vault.Path(), "key keyring", "1 secret(s)", "1 retired"} {
			if !strings.Contains(c.Detail, want) {
				t.Errorf("detail %q lacks %q", c.Detail, want)
			}
		}
		if strings.Contains(c.Detail, "sk-test") {
			t.Error("doctor printed a secret")
		}
	})
}
