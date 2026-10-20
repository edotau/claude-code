package secrets

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestDoctorMissingVaultIgnoresLegacyFiles(t *testing.T) {
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
		"no keyring tool on PATH",
		"set HARNESS_VAULT_KEY or the vault key_command",
		"secrets doctor: clean.",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("Doctor missing %q: %s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "legacy ") || strings.Contains(out.String(), "legacy-content-not-to-print") {
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

func TestDoctorRetiredKeys(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("fake keyring needs a POSIX shell")
	}
	vaulttest.UseKeyring(t)
	if _, err := vault.EnsureKey(); err != nil {
		t.Fatal(err)
	}
	kid, _, err := vault.RotateKey()
	if err != nil {
		t.Fatal(err)
	}
	var id string
	if err := vault.View(func(tx *vault.Tx) error {
		id, _ = tx.Get(vault.Meta, "vault_id")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	tool := "secret-tool"
	miss := "1"
	if runtime.GOOS == "darwin" {
		tool, miss = "security", "44"
	}
	script := "#!/bin/sh\ncase \"$*\" in *\"$EXPECTED_ACCOUNT\"*) ;; *) echo wrong-account >&2; exit 2;; esac\n" +
		"case \"$RETIRED_MODE\" in retained) echo fake-key;; missing) exit " + miss + ";; error) echo lookup-failed >&2; exit 2;; esac\n"
	if err := os.WriteFile(filepath.Join(dir, tool), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("EXPECTED_ACCOUNT", "vault-key/"+id+"."+kid)
	for _, tc := range []struct{ mode, want string }{
		{"retained", "✓ retired key " + kid + " retained"},
		{"missing", "⚠ retired key " + kid + " missing from the keyring"},
		{"error", "⚠ retired key " + kid + ": keyring: unavailable: lookup-failed"},
	} {
		t.Setenv("RETIRED_MODE", tc.mode)
		var out bytes.Buffer
		if rc := Doctor(&out, &providers.Registry{}); rc != ExitClean {
			t.Fatalf("Doctor = %d: %s", rc, &out)
		}
		if !strings.Contains(out.String(), tc.want) {
			t.Errorf("missing %q: %s", tc.want, &out)
		}
	}
}

// TestDoctorUnreadableVaultIsFailure: a vault whose schema the running binary refuses to read must
// fail the gate, never read as "no credentials stored" (checkMasterKey -> NamesErr).
func TestDoctorUnreadableVaultIsFailure(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Meta, "schema", "3")
	var out bytes.Buffer
	if rc := Doctor(&out, &providers.Registry{}); rc != ExitFindings {
		t.Fatalf("Doctor = %d, want findings: %s", rc, out.String())
	}
	if !strings.Contains(out.String(), "vault is unreadable:") || !strings.Contains(out.String(), "schema 3") {
		t.Fatalf("Doctor missing unreadable-vault diagnostic: %s", out.String())
	}
}

// TestDoctorFlagsArchivePerms: a .rolledback-* vault archive and a manifest-listed .bak file
// readable/writable by anyone but their owner must each be flagged, a 0600 one must not.
func TestDoctorFlagsArchivePerms(t *testing.T) {
	dir := vaulttest.Use(t)
	if err := os.MkdirAll(filepath.Dir(vault.Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	loose := vault.Path() + ".rolledback-20260101-000000"
	if err := os.WriteFile(loose, []byte("archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	tight := vault.Path() + ".rolledback-20260102-000000"
	if err := os.WriteFile(tight, []byte("archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	bak := filepath.Join(dir, "providers.json.bak-20260101-000000")
	if err := os.WriteFile(bak, []byte("backup"), 0o644); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(dir, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(stateDir, "vault-migration.json")
	manifestJSON := `{"items":[{"backup":"` + strings.ReplaceAll(bak, `\`, `\\`) + `"}]}`
	if err := os.WriteFile(manifest, []byte(manifestJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if rc := Doctor(&out, &providers.Registry{}); rc != ExitFindings {
		t.Fatalf("Doctor = %d, want findings: %s", rc, out.String())
	}
	if !strings.Contains(out.String(), loose+" is 0644") {
		t.Errorf("Doctor missing loose archive finding: %s", out.String())
	}
	if strings.Contains(out.String(), tight+" is") {
		t.Errorf("Doctor flagged a 0600 archive: %s", out.String())
	}
	if !strings.Contains(out.String(), bak+" is 0644") {
		t.Errorf("Doctor missing loose manifest backup finding: %s", out.String())
	}
}
