package secrets

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestDoctorMissingVaultAndLegacyFiles(t *testing.T) {
	dir := vaulttest.Use(t)
	noKeyring(t)
	legacy := filepath.Join(dir, "env.d")
	if err := os.Mkdir(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"secrets.env", "secrets.meta.json"} {
		if err := os.WriteFile(filepath.Join(legacy, name), []byte("legacy-content-not-to-print"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if rc := Doctor(&out, &providers.Registry{}); rc != ExitClean {
		t.Fatalf("Doctor = %d, want clean: %s", rc, out.String())
	}
	for _, want := range []string{
		"no vault yet (nothing to check)",
		"legacy " + filepath.Join(legacy, "secrets.env") + " found; `claude-code config migrate` imports it",
		"legacy " + filepath.Join(legacy, "secrets.meta.json") + " found; `claude-code config migrate` imports it",
		"no keyring tool on PATH",
		"set HARNESS_VAULT_KEY or the vault key_command",
		"secrets doctor: clean.",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("Doctor missing %q: %s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "legacy-content-not-to-print") {
		t.Fatal("Doctor printed legacy file contents")
	}
	if _, err := os.Stat(vault.Path()); !os.IsNotExist(err) {
		t.Fatalf("Doctor created a vault: %v", err)
	}
}

func TestDoctorMasterKeySources(t *testing.T) {
	for _, source := range []string{"env", "keyring"} {
		t.Run(source, func(t *testing.T) {
			if source == "keyring" {
				vaulttest.UseKeyring(t)
			} else {
				vaulttest.Use(t)
			}
			const value = "sk-test-abcdefghijklmnopqrstuvwxyz"
			if err := Store("DOCTOR_KEY", value); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			if rc := Doctor(&out, &providers.Registry{}); rc != ExitClean {
				t.Fatalf("Doctor = %d, want clean: %s", rc, out.String())
			}
			if !strings.Contains(out.String(), "1 secret(s); master key from "+source) ||
				!strings.Contains(out.String(), vault.Path()+" is 0600") {
				t.Fatalf("Doctor missing key source or vault permissions: %s", out.String())
			}
			if strings.Contains(out.String(), value) {
				t.Fatal("Doctor printed a secret")
			}
		})
	}
}

func TestDoctorLockedVault(t *testing.T) {
	for _, mode := range []string{"wrong key", "unavailable keyring"} {
		t.Run(mode, func(t *testing.T) {
			vaulttest.Use(t)
			const value = "sk-test-abcdefghijklmnopqrstuvwxyz"
			if err := Store("LOCKED", value); err != nil {
				t.Fatal(err)
			}
			want := "master key does not match"
			if mode == "wrong key" {
				wrongVaultKey(t)
			} else {
				kr := vaulttest.Keyring(t)
				kr.Down = true
				t.Setenv("HARNESS_VAULT_KEY", "")
				want = "keyring: unavailable"
			}
			var out bytes.Buffer
			if rc := Doctor(&out, &providers.Registry{}); rc != ExitFindings {
				t.Fatalf("Doctor = %d, want findings: %s", rc, out.String())
			}
			if !strings.Contains(out.String(), "vault holds 1 secret(s) but the master key does not open it:") ||
				!strings.Contains(out.String(), want) {
				t.Fatalf("Doctor missing locked-vault diagnostic: %s", out.String())
			}
			if strings.Contains(out.String(), value) {
				t.Fatal("Doctor printed a secret")
			}
		})
	}
}
