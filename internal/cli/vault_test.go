package cli

import (
	"bytes"
	"encoding/base64"
	"os"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/tailscale"
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
		{"show extra arg", []string{"show", "x"}, 2},
		{"edit extra arg", []string{"edit", "x"}, 2},
		{"show", []string{"show"}, 0},
		{"migrate is unknown", []string{"migrate"}, 2},
		{"finalize is unknown", []string{"finalize"}, 2},
		{"rollback is unknown", []string{"rollback"}, 2},
		{"rotate-key extra arg", []string{"rotate-key", "x"}, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vaulttest.Use(t)
			if got := cmdVault(tt.args); got != tt.want {
				t.Errorf("cmdVault(%q) = %d, want %d", tt.args, got, tt.want)
			}
		})
	}
}

func TestVaultVerbsAndMenu(t *testing.T) {
	vaulttest.Use(t)
	want := []string{"show", "set", "edit", "rotate-key", "key"}
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
	count := 0
	for _, c := range commands() {
		if c.name != "vault" {
			continue
		}
		count++
		if c.group != groupAuth {
			t.Errorf("vault group = %q, want %q", c.group, groupAuth)
		}
		if n := utf8.RuneCountInString(c.summary); n > summaryMax {
			t.Errorf("vault summary has %d characters, max %d", n, summaryMax)
		}
		if c.hidden || c.run == nil {
			t.Error("vault menu row is hidden or has no handler")
		}
	}
	if count != 1 {
		t.Errorf("commands() has %d vault rows, want 1", count)
	}
}

// TestVaultMigrateIsUnknownSubcommand: env.d support is gone (D2) — "vault migrate" is no longer a
// verb at all, not a no-op; it fails the same way any unrecognized subcommand does.
func TestVaultMigrateIsUnknownSubcommand(t *testing.T) {
	vaulttest.Use(t)
	code, _, stderr := captureOutput(t, func() int { return cmdVault([]string{"migrate"}) })
	if code != 2 {
		t.Fatalf("vault migrate = %d, want 2; stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, `unknown subcommand "migrate"`) {
		t.Fatalf("vault migrate stderr = %q, want it to name the unknown subcommand", stderr)
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

func TestCmdVaultShowTailscalePresenceOnly(t *testing.T) {
	vaulttest.Use(t)
	for _, name := range []string{
		"TAILSCALE_OAUTH_CLIENT_ID", "TAILSCALE_OAUTH_CLIENT_SECRET",
		"TAILSCALE_API_KEY", "TAILSCALE_AUTHKEY",
	} {
		t.Setenv(name, "")
	}
	const value = "known-tailscale-api-key-value"
	if err := secrets.Store("TAILSCALE_API_KEY", value); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := captureOutput(t, func() int {
		return cmdVault([]string{"show", "--tailscale"})
	})
	if code != 0 {
		t.Fatalf("vault show --tailscale = %d, stderr = %q", code, stderr)
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
	vaulttest.Use(t)
	vaultTestStdin(t, "id-value\nsecret-value\n")
	code, _, stderr := captureOutput(t, func() int {
		return cmdVault([]string{"set", "--tailscale"})
	})
	if code != 0 {
		t.Fatalf("vault set --tailscale = %d, stderr = %q", code, stderr)
	}
	for name, want := range map[string]string{
		"TAILSCALE_OAUTH_CLIENT_ID":     "id-value",
		"TAILSCALE_OAUTH_CLIENT_SECRET": "secret-value",
	} {
		value, source, ok := secrets.Get(name)
		if !ok || value != want || source != "vault" {
			t.Errorf("secrets.Get(%q) = (%q, %q, %v), want (%q, vault, true)", name, value, source, ok, want)
		}
	}
}

func TestCmdVaultSetTailscaleApiKeyOneLine(t *testing.T) {
	vaulttest.Use(t)
	vaultTestStdin(t, "apikeyvalue\n")
	code, _, stderr := captureOutput(t, func() int {
		return cmdVault([]string{"set", "--tailscale", "--api-key"})
	})
	if code != 0 {
		t.Fatalf("vault set --tailscale --api-key = %d, stderr = %q", code, stderr)
	}
	value, source, ok := secrets.Get("TAILSCALE_API_KEY")
	if !ok || value != "apikeyvalue" || source != "vault" {
		t.Errorf("secrets.Get(TAILSCALE_API_KEY) = (%q, %q, %v), want (apikeyvalue, vault, true)", value, source, ok)
	}
}

func TestCmdVaultSetTailscaleEmptyValueFails(t *testing.T) {
	vaulttest.Use(t)
	for _, tc := range []struct {
		name  string
		input string
	}{
		{"both empty", "\n\n"},
		{"empty id", "\nsecret-value\n"},
		{"empty secret", "id-value\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vaulttest.Use(t)
			vaultTestStdin(t, tc.input)
			code, _, stderr := captureOutput(t, func() int {
				return cmdVault([]string{"set", "--tailscale"})
			})
			if code != 1 {
				t.Fatalf("vault set --tailscale = %d, want 1; stderr = %q", code, stderr)
			}
			names := secrets.Names()
			for _, name := range []string{"TAILSCALE_OAUTH_CLIENT_ID", "TAILSCALE_OAUTH_CLIENT_SECRET"} {
				if slices.Contains(names, name) {
					t.Errorf("empty value stored %s", name)
				}
			}
		})
	}
}

func TestCmdVaultSetTailscaleDryRunStoresNothing(t *testing.T) {
	vaulttest.Use(t)
	vaultTestStdin(t, "id-value\nsecret-value\n")
	code, stdout, stderr := captureOutput(t, func() int {
		return cmdVault([]string{"set", "--tailscale", "--dry-run"})
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
	names := secrets.Names()
	for _, name := range []string{"TAILSCALE_OAUTH_CLIENT_ID", "TAILSCALE_OAUTH_CLIENT_SECRET"} {
		if slices.Contains(names, name) {
			t.Errorf("dry-run stored %s", name)
		}
	}
}

func TestCmdVaultSetBareExitsTwo(t *testing.T) {
	vaulttest.Use(t)
	code, _, stderr := captureOutput(t, func() int {
		return cmdVault([]string{"set"})
	})
	if code != 2 {
		t.Errorf("vault set = %d, want 2", code)
	}
	if !strings.Contains(stderr, "secrets set") {
		t.Errorf("stderr = %q, want guidance to use secrets set", stderr)
	}
}

func TestConfigVerbIsUnknown(t *testing.T) {
	vaulttest.Use(t)
	code, _, stderr := captureOutput(t, func() int { return Run([]string{"claude-code", "config", "show"}) })
	if code != 2 {
		t.Errorf("Run([config show]) = %d, want 2 (unknown verb)", code)
	}
	if !strings.Contains(stderr, `unknown command "config"`) {
		t.Errorf("stderr = %q, want it to name config as unknown", stderr)
	}
}

// A value spanning lines (embedded \r or \n) must be refused before any write, even when it is
// the second of a pair — secrets.Store would reject it anyway, but half a pair must never land.
func TestVaultStoreTailscaleRefusesEmbeddedNewline(t *testing.T) {
	for _, tc := range []struct {
		name  string
		pairs []vaultPair
	}{
		{"carriage return in first", []vaultPair{
			{Name: "TAILSCALE_OAUTH_CLIENT_ID", Value: "id-\rvalue"},
			{Name: "TAILSCALE_OAUTH_CLIENT_SECRET", Value: "secret-value"},
		}},
		{"newline in second", []vaultPair{
			{Name: "TAILSCALE_OAUTH_CLIENT_ID", Value: "id-value"},
			{Name: "TAILSCALE_OAUTH_CLIENT_SECRET", Value: "secret-\nvalue"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vaulttest.Use(t)
			code, _, stderr := captureOutput(t, func() int {
				return vaultStoreTailscale(false, tc.pairs)
			})
			if code != 1 {
				t.Fatalf("vaultStoreTailscale = %d, want 1; stderr = %q", code, stderr)
			}
			if !strings.Contains(stderr, "spans lines; nothing written") {
				t.Errorf("stderr = %q, want the spans-lines refusal", stderr)
			}
			names := secrets.Names()
			for _, p := range tc.pairs {
				if slices.Contains(names, p.Name) {
					t.Errorf("a value spanning lines stored %s even though it was refused", p.Name)
				}
			}
		})
	}
}

func TestTailscaleAPIVerdict(t *testing.T) {
	cred := func(id, sec, api bool) []tailscale.CredState {
		return []tailscale.CredState{{Present: id}, {Present: sec}, {Present: api}}
	}
	for _, tc := range []struct {
		name         string
		id, sec, api bool
		want         string
	}{
		{"full oauth", true, true, false, "oauth"},
		{"full oauth plus api key", true, true, true, "oauth"},
		{"api key only", false, false, true, "api-key"},
		{"half oauth id plus api key falls back to api-key", true, false, true, "api-key"},
		{"half oauth secret plus api key falls back to api-key", false, true, true, "api-key"},
		{"half oauth id only", true, false, false, "incomplete (need both …_ID and _SECRET)"},
		{"half oauth secret only", false, true, false, "incomplete (need both …_ID and _SECRET)"},
		{"none", false, false, false, "none"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tailscaleAPIVerdict(cred(tc.id, tc.sec, tc.api)); got != tc.want {
				t.Errorf("tailscaleAPIVerdict(id=%v,sec=%v,api=%v) = %q, want %q", tc.id, tc.sec, tc.api, got, tc.want)
			}
		})
	}
}

// TestCmdVaultRotateKeyRefusesWithEnvSetWithoutForce: the CLI layer surfaces vault.RotateKey's
// guard (via configRotateKey) as exit 1 with the guard's message, and --force bypasses it.
func TestCmdVaultRotateKeyRefusesWithEnvSetWithoutForce(t *testing.T) {
	dir, _ := vaulttest.UseKeyring(t)
	_ = dir
	if _, err := vault.EnsureKey(); err != nil {
		t.Fatal(err)
	}
	// A key_command (rather than env) is simplest to set up at this layer: present without
	// --force refuses, present with --force rotates.
	vaulttest.Put(t, vault.Meta, "key_command", `["/bin/true"]`)

	code, _, stderr := captureOutput(t, func() int { return cmdVault([]string{"rotate-key"}) })
	if code != 1 {
		t.Fatalf("vault rotate-key = %d, want 1; stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "key_command") {
		t.Errorf("stderr = %q, want it to name key_command", stderr)
	}

	code, _, stderr = captureOutput(t, func() int { return cmdVault([]string{"rotate-key", "--force"}) })
	if code != 0 {
		t.Fatalf("vault rotate-key --force = %d, want 0; stderr = %q", code, stderr)
	}
}

// TestCmdVaultKeyShowNonTTYDecodesToMasterKey: under go test, stdout is a pipe (captureOutput),
// never a terminal, so only the non-refusal path is reachable here — matching this repo's
// existing convention for TTY-gated code (tui.TestListNonTTY, tui.TestOpenFailsCleanlyWithoutATTY):
// the refusal itself, gated on tui.StdoutIsTerminal, is not exercised by an automated test.
func TestCmdVaultKeyShowNonTTYDecodesToMasterKey(t *testing.T) {
	vaulttest.Use(t)
	code, stdout, stderr := captureOutput(t, func() int { return cmdVault([]string{"key", "show"}) })
	if code != 0 {
		t.Fatalf("vault key show = %d, want 0; stderr = %q", code, stderr)
	}
	got, err := base64.StdEncoding.DecodeString(strings.TrimSpace(stdout))
	if err != nil {
		t.Fatalf("vault key show stdout %q does not decode as base64: %v", stdout, err)
	}
	if len(got) != 32 {
		t.Errorf("decoded master key is %d bytes, want 32", len(got))
	}
	want, err := base64.StdEncoding.DecodeString(os.Getenv("HARNESS_VAULT_KEY"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("vault key show printed a different key than HARNESS_VAULT_KEY")
	}
}

func TestVaultInAuthGroupHelp(t *testing.T) {
	rows := menuRows()
	group := ""
	found := false
	for _, r := range rows {
		if r.head != "" {
			group = r.head
			continue
		}
		if strings.TrimSpace(r.name) == "vault" {
			found = true
			if group != groupAuth {
				t.Errorf("vault appears under menu group %q, want %q", group, groupAuth)
			}
		}
	}
	if !found {
		t.Fatal("menuRows() has no vault row")
	}
}
