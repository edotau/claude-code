package secrets

import (
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func doctorTestVault(t *testing.T) string {
	t.Helper()
	dir := vaulttest.Use(t)
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARNESS_VAULT_KEY", base64.StdEncoding.EncodeToString(key))
	return dir
}

func doctorTestResult(reg *providers.Registry) (int, string) {
	rc := ExitClean
	var details []string
	for _, c := range DoctorChecks(reg) {
		if c.Gate && !c.OK {
			rc = ExitFindings
		}
		details = append(details, c.Name+": "+c.Detail)
	}
	return rc, strings.Join(details, "\n")
}

func TestDoctorMissingVaultIgnoresLegacyFiles(t *testing.T) {
	dir := doctorTestVault(t)
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
	rc, out := doctorTestResult(&providers.Registry{})
	if rc != ExitClean {
		t.Fatalf("DoctorChecks = %d, want clean: %s", rc, out)
	}
	for _, want := range []string{
		"no vault yet (nothing to check)",
		"none on PATH",
		"set HARNESS_VAULT_KEY or the vault key_command",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("DoctorChecks missing %q: %s", want, out)
		}
	}
	if strings.Contains(out, "legacy ") || strings.Contains(out, "legacy-content-not-to-print") {
		t.Fatal("Doctor printed legacy file contents")
	}
	if _, err := os.Stat(vault.Path()); !os.IsNotExist(err) {
		t.Fatalf("Doctor created a vault: %v", err)
	}
}

// TestMasterKeyStatus exercises MasterKeyStatus directly (the seam shared by checkMasterKey and
// launch's "vault" row): unreadable vault, empty vault, a good key, and a key that won't open it.
func TestMasterKeyStatus(t *testing.T) {
	t.Run("unreadable vault", func(t *testing.T) {
		doctorTestVault(t)
		vaulttest.Put(t, vault.Meta, "schema", strconv.Itoa(vault.SchemaVersion+1))
		count, source, err := MasterKeyStatus()
		if count != 0 || source != "" || err == nil {
			t.Fatalf("MasterKeyStatus = (%d, %q, %v), want (0, \"\", err)", count, source, err)
		}
	})
	t.Run("empty vault", func(t *testing.T) {
		doctorTestVault(t)
		count, source, err := MasterKeyStatus()
		if count != 0 || source != "not needed" || err != nil {
			t.Fatalf("MasterKeyStatus = (%d, %q, %v), want (0, \"not needed\", nil)", count, source, err)
		}
	})
	t.Run("secrets and good key", func(t *testing.T) {
		doctorTestVault(t)
		if err := Store("STATUS_KEY", "sk-test-abcdefghijklmnopqrstuvwxyz"); err != nil {
			t.Fatal(err)
		}
		count, source, err := MasterKeyStatus()
		if count != 1 || source != "env" || err != nil {
			t.Fatalf("MasterKeyStatus = (%d, %q, %v), want (1, \"env\", nil)", count, source, err)
		}
	})
	t.Run("secrets and bad key", func(t *testing.T) {
		doctorTestVault(t)
		if err := Store("STATUS_KEY", "sk-test-abcdefghijklmnopqrstuvwxyz"); err != nil {
			t.Fatal(err)
		}
		wrongVaultKey(t)
		count, source, err := MasterKeyStatus()
		if count != 1 || err == nil {
			t.Fatalf("MasterKeyStatus = (%d, %q, %v), want (1, _, err)", count, source, err)
		}
	})
}

func TestDoctorMasterKeySources(t *testing.T) {
	for _, source := range []string{"env", "keyring"} {
		t.Run(source, func(t *testing.T) {
			if source == "keyring" {
				vaulttest.UseKeyring(t)
			} else {
				doctorTestVault(t)
			}
			const value = "sk-test-abcdefghijklmnopqrstuvwxyz"
			if err := Store("DOCTOR_KEY", value); err != nil {
				t.Fatal(err)
			}
			rc, out := doctorTestResult(&providers.Registry{})
			if rc != ExitClean {
				t.Fatalf("Doctor = %d, want clean: %s", rc, out)
			}
			if !strings.Contains(out, "1 secret(s); master key from "+source) ||
				!strings.Contains(out, vault.Path()+" is 0600") {
				t.Fatalf("Doctor missing key source or vault permissions: %s", out)
			}
			if strings.Contains(out, value) {
				t.Fatal("Doctor printed a secret")
			}
		})
	}
}

func TestDoctorLockedVault(t *testing.T) {
	for _, mode := range []string{"wrong key", "unavailable keyring"} {
		t.Run(mode, func(t *testing.T) {
			doctorTestVault(t)
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
			rc, out := doctorTestResult(&providers.Registry{})
			if rc != ExitFindings {
				t.Fatalf("Doctor = %d, want findings: %s", rc, out)
			}
			if !strings.Contains(out, "vault holds 1 secret(s) but the master key does not open it:") ||
				!strings.Contains(out, want) {
				t.Fatalf("Doctor missing locked-vault diagnostic: %s", out)
			}
			if strings.Contains(out, value) {
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
	t.Setenv("HARNESS_KEYRING", "") // the audit pass forces it off; this test fakes the tool
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
		{"retained", "vault retired key " + kid + ": retained"},
		{"missing", "vault retired key " + kid + ": missing from the keyring"},
		{"error", "vault retired key " + kid + ": keyring: unavailable: lookup-failed"},
	} {
		t.Setenv("RETIRED_MODE", tc.mode)
		rc, out := doctorTestResult(&providers.Registry{})
		if rc != ExitClean {
			t.Fatalf("Doctor = %d: %s", rc, out)
		}
		if !strings.Contains(out, tc.want) {
			t.Errorf("missing %q: %s", tc.want, out)
		}
	}
}

// TestDoctorUnreadableVaultIsFailure: a vault whose schema the running binary refuses to read must
// fail the gate, never read as "no credentials stored" (checkMasterKey -> NamesErr).
func TestDoctorUnreadableVaultIsFailure(t *testing.T) {
	doctorTestVault(t)
	vaulttest.Put(t, vault.Meta, "schema", strconv.Itoa(vault.SchemaVersion+1))
	rc, out := doctorTestResult(&providers.Registry{})
	if rc != ExitFindings {
		t.Fatalf("Doctor = %d, want findings: %s", rc, out)
	}
	if !strings.Contains(out, "vault is unreadable:") || !strings.Contains(out, "schema "+strconv.Itoa(vault.SchemaVersion+1)) {
		t.Fatalf("Doctor missing unreadable-vault diagnostic: %s", out)
	}
}

// TestDoctorFlagsArchivePerms: a .rolledback-* vault archive and a manifest-listed .bak file
// readable/writable by anyone but their owner must each be flagged, a 0600 one must not.
func TestDoctorFlagsArchivePerms(t *testing.T) {
	dir := doctorTestVault(t)
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
	rc, out := doctorTestResult(&providers.Registry{})
	if rc != ExitFindings {
		t.Fatalf("Doctor = %d, want findings: %s", rc, out)
	}
	if !strings.Contains(out, loose+" is 0644") {
		t.Errorf("Doctor missing loose archive finding: %s", out)
	}
	if strings.Contains(out, tight+" is") {
		t.Errorf("Doctor flagged a 0600 archive: %s", out)
	}
	if !strings.Contains(out, bak+" is 0644") {
		t.Errorf("Doctor missing loose manifest backup finding: %s", out)
	}
}
