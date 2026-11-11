package doctor

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/edotau/claude-code/internal/gemini" // registers the gemini route engine the prod binary links
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestStrayClaudeJSONRejectsNonENOENTStatFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	hosttest.SymlinkLoop(t, filepath.Join(home, ".claude"))
	stub := filepath.Join(home, ".claude", ".claude.json")
	ok, detail := strayClaudeJSON()
	if ok || !strings.Contains(detail, "cannot stat "+stub) {
		t.Fatalf("stray config probe = %v, %q; want unhealthy cannot-stat diagnostic", ok, detail)
	}
}

func TestFormatsCheckFailsOnUnreadableStore(t *testing.T) {
	hermetic(t, "")
	if err := os.MkdirAll(filepath.Dir(vault.Path()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(vault.Path(), bytes.Repeat([]byte("not a bolt db"), 400), 0o600); err != nil {
		t.Fatal(err)
	}
	c := formatsCheck()
	if c.OK || !strings.Contains(c.Detail, "unreadable") {
		t.Fatalf("formats row = %+v, want failing with 'unreadable'", c)
	}
}

func TestStrayClaudeJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if ok, _ := strayClaudeJSON(); !ok {
		t.Fatal("no stub must pass")
	}
	stub := filepath.Join(home, ".claude", ".claude.json")
	if err := os.MkdirAll(filepath.Dir(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stub, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, detail := strayClaudeJSON(); ok || !strings.Contains(detail, stub) {
		t.Errorf("stub present: ok=%v detail=%q", ok, detail)
	}
}

func TestVaultDoctorChecks(t *testing.T) {
	t.Run("missing vault", testVaultDoctorMissing)
	t.Run("secrets and retired", testVaultDoctorSecrets)
	t.Run("corrupt retired metadata", testVaultDoctorCorrupt)
	t.Run("one vault row, not two", testVaultDoctorSingleRow)
}

func testVaultDoctorMissing(t *testing.T) {
	t.Helper()
	hermetic(t, "")
	checks := vaultDoctorChecks()
	c := findCheck(checks, "vault")
	if c == nil || c.Detail != "not created yet" || !c.OK {
		t.Errorf("vault row = %+v", c)
	}
	for _, name := range []string{"vault perms", "keyring tool"} {
		if c := findCheck(checks, name); c == nil || !c.OK {
			t.Errorf("missing healthy %s row", name)
		}
	}
}

func testVaultDoctorSecrets(t *testing.T) {
	t.Helper()
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
}

func testVaultDoctorCorrupt(t *testing.T) {
	t.Helper()
	hermetic(t, "")
	vaulttest.Put(t, vault.Meta, "retired", `["old", 42]`)
	checks := vaultDoctorChecks()
	for _, name := range []string{"vault", "vault retired keys"} {
		c := findCheck(checks, name)
		if c == nil || c.OK || !strings.Contains(c.Detail, "retired key IDs are corrupt") {
			t.Errorf("%s row = %+v, want corruption diagnostic", name, c)
		}
	}
}

func testVaultDoctorSingleRow(t *testing.T) {
	t.Helper()
	hermetic(t, "")
	t.Setenv("HARNESS_VAULT_KEY", "")
	vaulttest.Keyring(t)
	vaulttest.Secret(t, "FOO_API_KEY", "sk-test-0123456789")
	n := 0
	for _, c := range vaultDoctorChecks() {
		if c.Name == "vault" || c.Name == "vault master key" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("vaultDoctorChecks has %d vault/master-key row(s), want exactly 1 (the \"vault\" row carries the master-key status)", n)
	}
}

// seed writes a file under state/harness and returns its path.
func seedHarness(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(paths.StateDir(), name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFormatsDoctorRowIgnoresBareMapCaches(t *testing.T) {
	hermetic(t, "")
	seedHarness(t, "quota.json", `{"acct":{"until":"2099-01-01T00:00:00Z"}}`)
	seedHarness(t, "agentversion.json", `{"claude":"1.0.0"}`) // unversioned cache: a miss, never stale

	formats := findCheck([]Check{formatsCheck()}, "formats")
	if formats == nil || !formats.OK || strings.Contains(formats.Detail, "stale") { // quota stays a bare map: an old daemon reads it
		t.Fatalf("formats row = %+v, want current", formats)
	}
}
