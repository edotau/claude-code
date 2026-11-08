package vaultcmd

import (
	"bytes"
	"github.com/edotau/claude-code/internal/hosttest"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/network/tailscale"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestCmdVault(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want int
	}{
		{"nil", nil, 0},
		{"short help", []string{"-h"}, 0},
		{"long help", []string{"--help"}, 0},
		{"unknown", []string{"nope"}, 2},
		{"list unknown section", []string{"list", "bogus"}, 2},
		{"edit extra arg", []string{"edit", "x"}, 2},
		{"list", []string{"list"}, 0},
		{"show removed", []string{"show"}, 2},
		{"finalize is unknown", []string{"finalize"}, 2},
		{"rollback is unknown", []string{"rollback"}, 2},
		{"rotate-secret extra arg", []string{"rotate-secret", "x"}, 2},
		{"rotate-key removed", []string{"rotate-key"}, 2},
		{"key removed", []string{"key", "show"}, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vaulttest.UseFreshKey(t)
			if got := Run(tt.args); got != tt.want {
				t.Errorf("Run(%q) = %d, want %d", tt.args, got, tt.want)
			}
		})
	}
}

// vault edit has no flags or commands of its own; -h must print its own usage-only page, not vaultPage's
// COMMANDS list (every other vault -h path already covers the parent page itself).
func TestCmdVaultEditHelpPrintsOwnPage(t *testing.T) {
	vaulttest.UseFreshKey(t)
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return Run([]string{"edit", "-h"}) })
	if code != 0 {
		t.Fatalf("vault edit -h: exit %d, want 0", code)
	}
	got := stdout + stderr
	if !strings.HasPrefix(got, "usage: claude-code vault edit\n") {
		t.Errorf("vault edit -h: want its own usage line first:\n%s", got)
	}
	if strings.Contains(got, "COMMANDS") || strings.Contains(got, "claude-code vault <command>") {
		t.Errorf("vault edit -h: leaked the parent vaultPage:\n%s", got)
	}
}

func TestVaultVerbs(t *testing.T) {
	vaulttest.UseFreshKey(t)
	want := []string{"list", "set", "get", "delete", "import", "export", "edit", "rotate-secret"}
	if len(vaultVerbs) != len(want) {
		t.Errorf("vaultVerbs has %d entries, want exactly %v", len(vaultVerbs), want)
	}
	for _, name := range want {
		t.Run(name, func(t *testing.T) {
			if vaultVerbs[name] == nil {
				t.Errorf("vaultVerbs[%q] missing or nil", name)
			}
		})
	}
}

func vaultTestStdin(t *testing.T, input string) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	t.Cleanup(func() {
		os.Stdin = old
		f.Close()
	})
	if _, err := f.WriteString(input); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	os.Stdin = f
}

func TestCmdVaultListTailscalePresenceOnly(t *testing.T) {
	vaulttest.UseFreshKey(t)
	for _, name := range []string{
		"TAILSCALE_OAUTH_CLIENT_ID", "TAILSCALE_OAUTH_CLIENT_SECRET",
		"TAILSCALE_API_KEY", "TAILSCALE_AUTHKEY",
	} {
		t.Setenv(name, "")
	}
	const value = "known-tailscale-api-key-value"
	if err := vault.Store("TAILSCALE_API_KEY", value); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int {
		return Run([]string{"list", "--tailscale"})
	})
	if code != 0 {
		t.Fatalf("vault list --tailscale = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "TAILSCALE_API_KEY  set (") {
		t.Errorf("stdout = %q, want stored credential presence", stdout)
	}
	if strings.Contains(stdout, value) {
		t.Error("stdout contains the raw secret value")
	}
	if strings.Contains(stdout, "chars)") {
		t.Error("stdout contains a secret character count")
	}
	want := "TAILSCALE_OAUTH_CLIENT_ID  missing\n" +
		"TAILSCALE_OAUTH_CLIENT_SECRET  missing\n" +
		"TAILSCALE_API_KEY  set (vault)\n" +
		"TAILSCALE_AUTHKEY  missing\n" +
		"api: api-key\n"
	if stdout != want {
		t.Errorf("stdout = %q, want presence-only output %q", stdout, want)
	}
}

func TestCmdVaultSetTailscalePipedTwoLines(t *testing.T) {
	vaulttest.UseFreshKey(t)
	vaultTestStdin(t, "id-value\nsecret-value\n")
	code, _, stderr := hosttest.CaptureOutput(t, func() int {
		return Run([]string{"set", "--tailscale"})
	})
	if code != 0 {
		t.Fatalf("vault set --tailscale = %d, stderr = %q", code, stderr)
	}
	for name, want := range map[string]string{
		"TAILSCALE_OAUTH_CLIENT_ID":     "id-value",
		"TAILSCALE_OAUTH_CLIENT_SECRET": "secret-value",
	} {
		value, source, ok := vault.Get(name)
		if !ok || vault.Redact(value) != vault.Redact(want) || source != "vault" {
			t.Errorf("%s was not stored correctly in the vault", name)
		}
	}
}

func TestCmdVaultSetTailscaleApiKeyOneLine(t *testing.T) {
	vaulttest.UseFreshKey(t)
	vaultTestStdin(t, "apikeyvalue\n")
	code, _, stderr := hosttest.CaptureOutput(t, func() int {
		return Run([]string{"set", "--tailscale", "--api-key"})
	})
	if code != 0 {
		t.Fatalf("vault set --tailscale --api-key = %d, stderr = %q", code, stderr)
	}
	value, source, ok := vault.Get("TAILSCALE_API_KEY")
	if !ok || vault.Redact(value) != vault.Redact("apikeyvalue") || source != "vault" {
		t.Error("TAILSCALE_API_KEY was not stored correctly in the vault")
	}
}

func TestCmdVaultSetTailscaleEmptyValueFails(t *testing.T) {
	vaulttest.UseFreshKey(t)
	for _, tc := range []struct {
		name  string
		input string
	}{
		{"both empty", "\n\n"},
		{"empty id", "\nsecret-value\n"},
		{"empty secret", "id-value\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vaulttest.UseFreshKey(t)
			vaultTestStdin(t, tc.input)
			code, _, stderr := hosttest.CaptureOutput(t, func() int {
				return Run([]string{"set", "--tailscale"})
			})
			if code != 1 {
				t.Fatalf("vault set --tailscale = %d, want 1; stderr = %q", code, stderr)
			}
			names := vault.Names()
			for _, name := range []string{"TAILSCALE_OAUTH_CLIENT_ID", "TAILSCALE_OAUTH_CLIENT_SECRET"} {
				if slices.Contains(names, name) {
					t.Errorf("empty value stored %s", name)
				}
			}
		})
	}
}

func TestCmdVaultSetTailscaleDryRunStoresNothing(t *testing.T) {
	vaulttest.UseFreshKey(t)
	vaultTestStdin(t, "id-value\nsecret-value\n")
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int {
		return Run([]string{"set", "--tailscale", "--dry-run"})
	})
	if code != 0 {
		t.Fatalf("vault set --tailscale --dry-run = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{
		"would add TAILSCALE_OAUTH_CLIENT_ID (8 chars)",
		"would add TAILSCALE_OAUTH_CLIENT_SECRET (12 chars)",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout = %q, want %q", stdout, want)
		}
	}
	names := vault.Names()
	for _, name := range []string{"TAILSCALE_OAUTH_CLIENT_ID", "TAILSCALE_OAUTH_CLIENT_SECRET"} {
		if slices.Contains(names, name) {
			t.Errorf("dry-run stored %s", name)
		}
	}
}

func TestCmdVaultSetBareExitsTwo(t *testing.T) {
	vaulttest.UseFreshKey(t)
	withStdin(t, "")
	code, _, stderr := hosttest.CaptureOutput(t, func() int {
		return Run([]string{"set"})
	})
	if code != 2 {
		t.Errorf("vault set = %d, want 2", code)
	}
	if !strings.Contains(stderr, "usage: claude-code vault set [NAME]") {
		t.Errorf("stderr = %q, want vault set usage", stderr)
	}
}

// A value spanning lines (embedded \r or \n) must be refused before any write, even when it is
// the second of a pair — vault.Store would reject it anyway, but half a pair must never land.
func TestVaultStoreTailscaleRefusesEmbeddedNewline(t *testing.T) {
	for _, tc := range []struct {
		name  string
		pairs []tailscale.Cred
	}{
		{"carriage return in first", []tailscale.Cred{
			{Name: "TAILSCALE_OAUTH_CLIENT_ID", Value: "id-\rvalue"},
			{Name: "TAILSCALE_OAUTH_CLIENT_SECRET", Value: "secret-value"},
		}},
		{"newline in second", []tailscale.Cred{
			{Name: "TAILSCALE_OAUTH_CLIENT_ID", Value: "id-value"},
			{Name: "TAILSCALE_OAUTH_CLIENT_SECRET", Value: "secret-\nvalue"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vaulttest.UseFreshKey(t)
			code, _, stderr := hosttest.CaptureOutput(t, func() int {
				return vaultStoreTailscale(false, tc.pairs)
			})
			if code != 1 {
				t.Fatalf("vaultStoreTailscale = %d, want 1; stderr = %q", code, stderr)
			}
			if !strings.Contains(stderr, "spans lines; nothing written") {
				t.Errorf("stderr = %q, want the spans-lines refusal", stderr)
			}
			names := vault.Names()
			for _, p := range tc.pairs {
				if slices.Contains(names, p.Name) {
					t.Errorf("a value spanning lines stored %s even though it was refused", p.Name)
				}
			}
		})
	}
}

func TestCmdVaultRotateSecretRefusesWithEnvSetWithoutForce(t *testing.T) {
	dir, _ := vaulttest.UseKeyring(t)
	_ = dir
	if _, err := vault.EnsureKey(); err != nil {
		t.Fatal(err)
	}
	// A key_command (rather than env) is simplest to set up at this layer: present without
	// --force refuses, present with --force rotates.
	vaulttest.Put(t, vault.Meta, "key_command", `["/bin/true"]`)

	code, _, stderr := hosttest.CaptureOutput(t, func() int { return Run([]string{"rotate-secret"}) })
	if code != 1 {
		t.Fatalf("vault rotate-secret = %d, want 1; stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "key_command") {
		t.Errorf("stderr = %q, want it to name key_command", stderr)
	}

	code, _, stderr = hosttest.CaptureOutput(t, func() int { return Run([]string{"rotate-secret", "--force"}) })
	if code != 0 {
		t.Fatalf("vault rotate-secret --force = %d, want 0; stderr = %q", code, stderr)
	}
}

// usageRow mirrors internal/ui/helptext's unexported row/isRow matcher — kept in sync by hand; if
// helptext's own regex changes, this test and the real painter can drift apart.
var usageRow = regexp.MustCompile(`^(\s{2,})(\S.*?)(\s{2,}\S.*)?$`)

func isUsageRow(l string) bool {
	m := usageRow.FindStringSubmatch(l)
	if m == nil || len(m[1]) > 4 {
		return false
	}
	return m[3] != "" || strings.HasPrefix(m[2], "-")
}

// TestVaultUsageRowsMatchHelptextRowRegex: every line naming a subcommand (indent <= 4, helptext's
// own cutoff) must satisfy helptext's row matcher, or the gradient paint silently skips it. A
// wrapped continuation line (indent > 4, no column of its own) carries no label and is exempt.
func TestVaultUsageRowsMatchHelptextRowRegex(t *testing.T) {
	var page bytes.Buffer
	vaultPage.Fprint(&page)
	lines := strings.Split(page.String(), "\n")
	if len(lines) < 2 {
		t.Fatal("vaultUsage has no subcommand lines")
	}
	sawRow := false
	for _, l := range lines[1:] {
		if !strings.HasPrefix(l, "  ") {
			continue
		}
		m := usageRow.FindStringSubmatch(l)
		if m == nil {
			t.Errorf("vaultUsage line %q does not even match the indented-line shape", l)
			continue
		}
		if len(m[1]) > 4 {
			continue // wrapped continuation line, no label column of its own
		}
		if !isUsageRow(l) {
			t.Errorf("vaultUsage line %q does not match helptext's row regex; it will not be painted", l)
			continue
		}
		sawRow = true
	}
	if !sawRow {
		t.Fatal("vaultUsage has no line helptext would paint as a row")
	}
}

// TestConfigVerbIsUnknown is also the only coverage of Run's unknown-command path (provider lookup → exit 2).

func TestVaultSetCopilotTokenRefusesShortValue(t *testing.T) {
	vaulttest.UseFreshKey(t)
	withStdin(t, "ok\n")

	code, _, stderr := hosttest.CaptureOutput(t, func() int { return Run([]string{"set", "COPILOT_GITHUB_TOKEN"}) })
	if code == 0 {
		t.Fatal("set COPILOT_GITHUB_TOKEN accepted a short value")
	}
	if !strings.Contains(stderr, "not a GitHub token") {
		t.Fatalf("stderr = %q, want the not-a-GitHub-token refusal", stderr)
	}
	if _, _, ok := vault.Get("COPILOT_GITHUB_TOKEN"); ok {
		t.Fatal("a refused value must not be stored")
	}
}

func TestVaultSetTailscaleCredentialRefusesMalformedShape(t *testing.T) {
	vaulttest.UseFreshKey(t)
	withStdin(t, "not-a-client-id\n")

	code, _, stderr := hosttest.CaptureOutput(t, func() int { return Run([]string{"set", "TAILSCALE_OAUTH_CLIENT_ID"}) })
	if code == 0 {
		t.Fatal("set TAILSCALE_OAUTH_CLIENT_ID accepted a malformed value")
	}
	if !strings.Contains(stderr, "want a client ID") {
		t.Fatalf("stderr = %q, want the client-ID shape refusal", stderr)
	}
	if _, _, ok := vault.Get("TAILSCALE_OAUTH_CLIENT_ID"); ok {
		t.Fatal("a refused value must not be stored")
	}
}

// A valid-shaped GitHub token is accepted and sealed in the vault.
func TestVaultSetCopilotTokenAcceptsValidShape(t *testing.T) {
	vaulttest.UseFreshKey(t)
	const tok = "gh" + "p_validshapedFAKEtoken1234567890"
	withStdin(t, tok+"\n")

	code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return Run([]string{"set", "COPILOT_GITHUB_TOKEN"}) })
	if code != 0 {
		t.Fatalf("set COPILOT_GITHUB_TOKEN exit = %d", code)
	}
	if v, _, ok := vault.Get("COPILOT_GITHUB_TOKEN"); !ok || vault.Redact(v) != vault.Redact(tok) {
		t.Fatal("COPILOT_GITHUB_TOKEN was not stored correctly")
	}
	if strings.Contains(stdout+stderr, tok) {
		t.Fatal("set leaked the raw value")
	}
	if !strings.Contains(stderr, vault.Redact(tok)) {
		t.Fatal("set did not report the masked value")
	}
}

func TestVaultSetNameDryRunStoresNothing(t *testing.T) {
	vaulttest.UseFreshKey(t)
	const value = "obviously-fake-dry-run"
	withStdin(t, value+"\n")
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int {
		return Run([]string{"set", "VAULT_TEST_DUMMY", "--dry-run"})
	})
	if code != 0 || !strings.Contains(stdout, "would add VAULT_TEST_DUMMY") {
		t.Fatalf("set --dry-run: exit %d, expected preview=%v",
			code, strings.Contains(stdout, "would add VAULT_TEST_DUMMY"))
	}
	if strings.Contains(stdout+stderr, value) {
		t.Fatal("dry run leaked the raw value")
	}
	if len(vault.Names()) != 0 {
		t.Fatal("dry run stored a secret")
	}
}

func TestVaultSetNameAndTailscaleRefused(t *testing.T) {
	vaulttest.UseFreshKey(t)
	withStdin(t, "obviously-fake-unused\n")
	code, _, _ := hosttest.CaptureOutput(t, func() int {
		return Run([]string{"set", "VAULT_TEST_DUMMY", "--tailscale"})
	})
	if code != 2 || len(vault.Names()) != 0 {
		t.Fatalf("set NAME --tailscale: exit %d, stored names=%v", code, vault.Names())
	}
}

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

func TestVaultStorageHelpPrintsUsage(t *testing.T) {
	vaulttest.UseFreshKey(t)
	for _, op := range []string{"set", "get", "delete", "import", "export"} {
		code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return Run([]string{op, "-h"}) })
		if code != 0 || !strings.Contains(stderr, "usage: claude-code vault "+op) || stdout != "" {
			t.Errorf("vault %s -h: want usage + exit 0 and no op output, got exit %d", op, code)
		}
	}
}
