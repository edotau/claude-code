package providers

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

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

func doctorTestResult(reg *Registry) (int, string) {
	rc := vault.ExitClean
	var details []string
	for _, c := range DoctorChecks(reg) {
		if c.Gate && !c.OK {
			rc = vault.ExitFindings
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
	rc, out := doctorTestResult(&Registry{})
	if rc != vault.ExitClean {
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

// A vault with no sealed secrets needs no master key, so checkMasterKey adds no row; the other states are Doctor tests.
func TestCheckMasterKeyEmptyVault(t *testing.T) {
	doctorTestVault(t)
	noKeyring(t)
	t.Setenv("HARNESS_VAULT_KEY", "")
	vaulttest.Put(t, vault.Pins, "HARNESS_PROVIDER", "openai")
	if rows := checkMasterKey(); len(rows) != 0 {
		t.Fatalf("checkMasterKey = %+v, want no row for a vault without secrets", rows)
	}
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
			if err := vault.Store("DOCTOR_KEY", value); err != nil {
				t.Fatal(err)
			}
			rc, out := doctorTestResult(&Registry{})
			if rc != vault.ExitClean {
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
			if err := vault.Store("LOCKED", value); err != nil {
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
			rc, out := doctorTestResult(&Registry{})
			if rc != vault.ExitFindings {
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

func TestDoctorCorruptRetiredKeys(t *testing.T) {
	doctorTestVault(t)
	vaulttest.Put(t, vault.Meta, "retired", `["old", 42]`)
	checks := checkRetiredKeys()
	if len(checks) != 1 {
		t.Fatalf("checkRetiredKeys = %+v, want one error row", checks)
	}
	c := checks[0]
	if c.Name != "vault retired keys" || c.OK || c.Gate || !strings.Contains(c.Detail, "retired key IDs are corrupt") {
		t.Fatalf("retired keys row = %+v, want advisory corruption diagnostic", c)
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
	fakeRetiredKeyTool(t, kid)
	for _, tc := range []struct{ mode, want string }{
		{"retained", "vault retired key " + kid + ": retained"},
		{"missing", "vault retired key " + kid + ": missing from the keyring"},
		{"error", "vault retired key " + kid + ": keyring: unavailable: lookup-failed"},
	} {
		t.Setenv("RETIRED_MODE", tc.mode)
		rc, out := doctorTestResult(&Registry{})
		if rc != vault.ExitClean {
			t.Fatalf("Doctor = %d: %s", rc, out)
		}
		if !strings.Contains(out, tc.want) {
			t.Errorf("missing %q: %s", tc.want, out)
		}
	}
}

// fakeRetiredKeyTool puts a fake keyring tool first on PATH that answers for the retired key kid per RETIRED_MODE.
func fakeRetiredKeyTool(t *testing.T, kid string) {
	t.Helper()
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
}

// TestDoctorUnreadableVaultIsFailure: a vault whose schema the running binary refuses to read must
// fail the gate, never read as "no credentials stored" (checkMasterKey -> NamesErr).
func TestDoctorUnreadableVaultIsFailure(t *testing.T) {
	doctorTestVault(t)
	vaulttest.Put(t, vault.Meta, "schema", strconv.Itoa(currentVaultSchema(t)+1))
	rc, out := doctorTestResult(&Registry{})
	if rc != vault.ExitFindings {
		t.Fatalf("Doctor = %d, want findings: %s", rc, out)
	}
	if !strings.Contains(out, "vault is unreadable:") || !strings.Contains(out, "schema "+strconv.Itoa(currentVaultSchema(t)+1)) {
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
	tight := vault.Path() + ".rolledback-20260102-000000"
	writeTestFile(t, loose, "archive", 0o644)
	writeTestFile(t, tight, "archive", 0o600)
	bak := filepath.Join(dir, "providers.json.bak-20260101-000000")
	writeTestFile(t, bak, "backup", 0o644)
	stateDir := filepath.Join(dir, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	manifestJSON := `{"items":[{"backup":"` + strings.ReplaceAll(bak, `\`, `\\`) + `"}]}`
	writeTestFile(t, filepath.Join(stateDir, "vault-migration.json"), manifestJSON, 0o600)
	rc, out := doctorTestResult(&Registry{})
	if rc != vault.ExitFindings {
		t.Fatalf("Doctor = %d, want findings: %s", rc, out)
	}
	if !strings.Contains(out, loose+" is 0644, want 0600 — run: chmod 600 "+loose) {
		t.Errorf("Doctor missing loose archive finding or its remediation: %s", out)
	}
	if strings.Contains(out, tight+" is") {
		t.Errorf("Doctor flagged a 0600 archive: %s", out)
	}
	if !strings.Contains(out, bak+" is 0644") {
		t.Errorf("Doctor missing loose manifest backup finding: %s", out)
	}
}

// writeTestFile writes content to path with perm, failing the test on error.
func writeTestFile(t *testing.T, path, content string, perm os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		t.Fatal(err)
	}
}

func TestKeyErrTextWithholdsKeyCommandStderr(t *testing.T) {
	got := keyErrText(errors.New("vault: key_command: exit status 1: leaked-secret-xyz"))
	if got != "vault: key_command: exit status 1 (stderr withheld)" {
		t.Fatalf("keyErrText = %q", got)
	}
	if got := keyErrText(errors.New("keyring: unavailable")); got != "keyring: unavailable" {
		t.Fatalf("keyErrText = %q", got)
	}
}

func TestConfigFixArchiveListingErrorIsFinding(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	// A regular file instead of a directory fails portably, even when tests run as root.
	state := filepath.Join(dir, "state")
	writeTestFile(t, state, "not a directory", 0o600)
	for _, row := range checkArchivePerms() {
		if row.Name == "vault archive perms" && strings.Contains(row.Detail, "could not list the vault directory") {
			if row.OK || !row.Gate || !strings.Contains(row.Detail, state) {
				t.Fatalf("listing error must fail the gate and identify its path: %+v", row)
			}
			return
		}
	}
	t.Fatal("archive audit omitted the vault directory listing error")
}

func noKeyring(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

func wrongVaultKey(t *testing.T) {
	t.Helper()
	vaulttest.Keyring(t)
	t.Setenv("HARNESS_VAULT_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)))
}

// Doctor flags a near miss (distance <= 2) of a known key as a typo but still passes the gate; a farther name is not.
func TestDoctorPassesOnCleanStore(t *testing.T) {
	doctorTestVault(t)
	vaulttest.Secret(t, "JIRA_TOKEN", "v")
	vaulttest.Secret(t, "OPENA_API_KEY", "v")
	vaulttest.Secret(t, "OPENXYZ_API_KEY", "v") // distance 3 from OPENAI_API_KEY
	reg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	rc, detail := doctorTestResult(reg)
	if rc != vault.ExitClean {
		t.Fatalf("DoctorChecks rc = %d, want %d:\n%s", rc, vault.ExitClean, detail)
	}
	if !strings.Contains(detail, "OPENA_API_KEY (did you mean OPENAI_API_KEY?)") ||
		strings.Contains(detail, "JIRA_TOKEN (") || strings.Contains(detail, "OPENXYZ_API_KEY (") ||
		!strings.Contains(detail, "3 secret(s); master key from env") {
		t.Fatalf("DoctorChecks missing typo advisory, flagged a far name, or lost key source: %s", detail)
	}
}
