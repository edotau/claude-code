package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/config"
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
		{"list unknown section", []string{"list", "bogus"}, 2},
		{"edit extra arg", []string{"edit", "x"}, 2},
		{"list", []string{"list"}, 0},
		{"show removed", []string{"show"}, 2},
		{"migrate is unknown", []string{"migrate"}, 2},
		{"finalize is unknown", []string{"finalize"}, 2},
		{"rollback is unknown", []string{"rollback"}, 2},
		{"rotate-secret extra arg", []string{"rotate-secret", "x"}, 2},
		{"rotate-key removed", []string{"rotate-key"}, 2},
		{"key removed", []string{"key", "show"}, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			useVaultTest(t)
			if got := cmdVault(tt.args); got != tt.want {
				t.Errorf("cmdVault(%q) = %d, want %d", tt.args, got, tt.want)
			}
		})
	}
}

func TestVaultVerbsAndMenu(t *testing.T) {
	useVaultTest(t)
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
	count := 0
	for _, c := range commands() {
		if c.name != "vault" {
			continue
		}
		count++
		if c.group != groupAuth {
			t.Errorf("vault group = %q, want %q", c.group, groupAuth)
		}
		if c.summary == "" {
			t.Error("vault summary is empty")
		}
		if c.hidden || c.run == nil {
			t.Error("vault menu row is hidden or has no handler")
		}
	}
	if count != 1 {
		t.Errorf("commands() has %d vault rows, want 1", count)
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
	useVaultTest(t)
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
		return cmdVault([]string{"list", "--tailscale"})
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
	useVaultTest(t)
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
		if !ok || secrets.Redact(value) != secrets.Redact(want) || source != "vault" {
			t.Errorf("%s was not stored correctly in the vault", name)
		}
	}
}

func TestCmdVaultSetTailscaleApiKeyOneLine(t *testing.T) {
	useVaultTest(t)
	vaultTestStdin(t, "apikeyvalue\n")
	code, _, stderr := captureOutput(t, func() int {
		return cmdVault([]string{"set", "--tailscale", "--api-key"})
	})
	if code != 0 {
		t.Fatalf("vault set --tailscale --api-key = %d, stderr = %q", code, stderr)
	}
	value, source, ok := secrets.Get("TAILSCALE_API_KEY")
	if !ok || secrets.Redact(value) != secrets.Redact("apikeyvalue") || source != "vault" {
		t.Error("TAILSCALE_API_KEY was not stored correctly in the vault")
	}
}

func TestCmdVaultSetTailscaleEmptyValueFails(t *testing.T) {
	useVaultTest(t)
	for _, tc := range []struct {
		name  string
		input string
	}{
		{"both empty", "\n\n"},
		{"empty id", "\nsecret-value\n"},
		{"empty secret", "id-value\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useVaultTest(t)
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
	useVaultTest(t)
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
	useVaultTest(t)
	withStdin(t, "")
	code, _, stderr := captureOutput(t, func() int {
		return cmdVault([]string{"set"})
	})
	if code != 2 {
		t.Errorf("vault set = %d, want 2", code)
	}
	if !strings.Contains(stderr, "usage: claude-code vault set [NAME]") {
		t.Errorf("stderr = %q, want vault set usage", stderr)
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
			useVaultTest(t)
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

func TestCmdVaultRotateSecretRefusesWithEnvSetWithoutForce(t *testing.T) {
	dir, _ := vaulttest.UseKeyring(t)
	_ = dir
	if _, err := vault.EnsureKey(); err != nil {
		t.Fatal(err)
	}
	// A key_command (rather than env) is simplest to set up at this layer: present without
	// --force refuses, present with --force rotates.
	vaulttest.Put(t, vault.Meta, "key_command", `["/bin/true"]`)

	code, _, stderr := captureOutput(t, func() int { return cmdVault([]string{"rotate-secret"}) })
	if code != 1 {
		t.Fatalf("vault rotate-secret = %d, want 1; stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "key_command") {
		t.Errorf("stderr = %q, want it to name key_command", stderr)
	}

	code, _, stderr = captureOutput(t, func() int { return cmdVault([]string{"rotate-secret", "--force"}) })
	if code != 0 {
		t.Fatalf("vault rotate-secret --force = %d, want 0; stderr = %q", code, stderr)
	}
}

func TestCmdVaultGetKeyNonTTYDecodesToMasterKey(t *testing.T) {
	useVaultTest(t)
	code, stdout, stderr := captureOutput(t, func() int { return cmdVault([]string{"get", "--key"}) })
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

// Captured output defaults to JSON with masked secrets alongside config.Export's sections.
func TestVaultListDefaultIsJSONUnderCaptureOutput(t *testing.T) {
	useVaultTest(t)
	seedVaultListFixture(t)
	want, err := config.Export()
	if err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := captureOutput(t, func() int { return cmdVault([]string{"list"}) })
	if code != 0 {
		t.Fatalf("vault list = %d, stderr = %q", code, stderr)
	}
	checkVaultListJSON(t, stdout, want)
}

// Explicit JSON preserves every non-secret config section and adds masked secret metadata.
func TestVaultListFormatJSONMatchesExport(t *testing.T) {
	useVaultTest(t)
	seedVaultListFixture(t)
	want, err := config.Export()
	if err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := captureOutput(t, func() int { return cmdVault([]string{"list", "--format", "json"}) })
	if code != 0 {
		t.Fatalf("vault list --format json = %d, stderr = %q", code, stderr)
	}
	checkVaultListJSON(t, stdout, want)
}

func checkVaultListJSON(t *testing.T, stdout string, exported []byte) {
	t.Helper()
	if strings.Contains(stdout, "tvly-dev-secret-leak-me-not") {
		t.Fatal("JSON leaked a raw secret")
	}
	var got, want map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(exported, &want); err != nil {
		t.Fatal(err)
	}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(got["secrets"], &rows); err != nil || len(rows) != 1 {
		t.Fatal("expected one masked secret row")
	}
	for _, row := range rows {
		if len(row) != 3 || row["name"] == nil || row["masked"] == nil || row["length"] == nil {
			t.Fatal("secret keys must be exactly length, masked, name")
		}
		var name, masked string
		var length int
		_ = json.Unmarshal(row["name"], &name)
		_ = json.Unmarshal(row["masked"], &masked)
		_ = json.Unmarshal(row["length"], &length)
		stored, err := secrets.ListStored(nil)
		if err != nil || len(stored) != 1 {
			t.Fatal("could not read fixture metadata")
		}
		if name != "TAVILY_API_KEY" || masked != stored[0].Masked() || length != stored[0].Len {
			t.Fatal("secret metadata differs from stored masked metadata")
		}
	}
	delete(got, "secrets")
	a, _ := json.Marshal(got)
	b, _ := json.Marshal(want)
	if !bytes.Equal(a, b) {
		t.Fatal("list JSON changed config.Export sections")
	}
}

// TestVaultListFormatBogusExitsTwo: an unrecognized --format value is a usage error, not a crash
// or a silent fallback.
func TestVaultListFormatBogusExitsTwo(t *testing.T) {
	useVaultTest(t)
	code, _, stderr := captureOutput(t, func() int { return cmdVault([]string{"list", "--format", "bogus"}) })
	if code != 2 {
		t.Fatalf("vault list --format bogus = %d, want 2; stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, `"bogus"`) {
		t.Errorf("stderr = %q, want it to name the bad --format value", stderr)
	}
}

// Seed every config section and a fake secret that must only appear masked.
func seedVaultListFixture(t *testing.T) {
	t.Helper()
	vaulttest.Put(t, vault.Pins, "HARNESS_PROVIDER", "anthropic")
	vaulttest.Put(t, vault.Env, "KEEP_ENV", "keepvalue")
	vaulttest.Put(t, vault.Overlay, "providers",
		`{"default":"anthropic","fallback":["one","two"],"providers":{"openai":null,"custom":{"models":{"opus":"m1"}}}}`)
	vaulttest.Put(t, vault.Overlay, "vendors", `{}`)
	vaulttest.Put(t, vault.Overlay, "settings", `{"env":{"FOO":"bar"},"flag":true}`)
	vaulttest.Put(t, vault.Meta, "key_command", `["pass","show","x"]`)
	vaulttest.Secret(t, "TAVILY_API_KEY", "tvly-dev-secret-leak-me-not")
}

// List renders providers as tables and keeps generic config rows without exposing values.
func TestVaultListTextFixtureFlattensEverySection(t *testing.T) {
	useVaultTest(t)
	seedVaultListFixture(t)
	code, stdout, stderr := captureOutput(t, func() int { return cmdVault([]string{"list", "--format", "text"}) })
	if strings.Contains(stdout+stderr, "tvly-dev-secret-leak-me-not") {
		t.Fatal("vault list leaked a raw secret value")
	}
	if code != 0 {
		t.Fatalf("vault list --format text = %d, stderr = %q", code, stderr)
	}
	for _, head := range []string{"PINS", "ENV", "PROVIDERS", "VENDORS", "SETTINGS", "VAULT"} {
		if !strings.Contains(stdout, head+"\n") {
			t.Errorf("stdout missing heading %q; stdout = %q", head, stdout)
		}
	}
	for _, tc := range []struct{ key, value string }{
		{"HARNESS_PROVIDER", "anthropic"}, // PINS
		{"KEEP_ENV", "keepvalue"},         // ENV
		{"default", "anthropic"},          // PROVIDERS top-level sibling of providers.*
		{"fallback", "one → two"},
		{"env.FOO", "bar"},                     // SETTINGS nested
		{"flag", "true"},                       // SETTINGS scalar
		{"key_command", `["pass","show","x"]`}, // VAULT
	} {
		if !vaultTextHasRow(stdout, tc.key, tc.value) {
			t.Errorf("stdout missing row %s=%s; stdout = %q", tc.key, tc.value, stdout)
		}
	}
	if !strings.Contains(stdout, "VENDORS\n  (none)\n") {
		t.Errorf("stdout = %q, want VENDORS rendered as (none) for an empty overlay", stdout)
	}
	for _, row := range []string{
		"PROVIDER KIND AUTH OPUS SONNET HAIKU OPENCODE FALLBACK",
		"custom m1", "openai (removed)",
	} {
		if !strings.Contains(strings.Join(strings.Fields(stdout), " "), row) {
			t.Errorf("missing provider table row %q", row)
		}
	}
}

// vaultTextHasRow reports whether the rendered table has a row for key with exactly value,
// tolerant of the column-alignment padding (vaultSection pads keys to the widest in the section).
func vaultTextHasRow(stdout, key, value string) bool {
	re := regexp.MustCompile(`(?m)^  ` + regexp.QuoteMeta(key) + `\s\s+` + regexp.QuoteMeta(value) + `$`)
	return re.MatchString(stdout)
}

// Empty sections render (none), including absent overlay documents.
func TestVaultListTextEmptyVaultAllSectionsNone(t *testing.T) {
	useVaultTest(t)
	code, stdout, stderr := captureOutput(t, func() int { return cmdVault([]string{"list", "--format", "text"}) })
	if code != 0 {
		t.Fatalf("vault list --format text = %d, stderr = %q", code, stderr)
	}
	for _, head := range []string{"SECRETS", "PINS", "ENV", "PROVIDERS", "VENDORS", "SETTINGS"} {
		if !strings.Contains(stdout, head+"\n  (none)\n") {
			t.Errorf("stdout = %q, want %s rendered as (none)", stdout, head)
		}
	}
	if !strings.Contains(stdout, "VAULT\n  key_command  (none)\n") {
		t.Errorf("stdout = %q, want VAULT's key_command row to read (none)", stdout)
	}
}

// TestVaultListTextTruncatesToWidth: a row longer than $COLUMNS is clipped to that many runes and
// ends in the ellipsis, rather than wrapping or overflowing the terminal.
func TestVaultListTextTruncatesToWidth(t *testing.T) {
	useVaultTest(t)
	vaulttest.Put(t, vault.Env, "LONG_ENV_NAME_FOR_WIDTH_TEST", strings.Repeat("x", 200))
	t.Setenv("COLUMNS", "40")
	code, stdout, stderr := captureOutput(t, func() int { return cmdVault([]string{"list", "--format", "text"}) })
	if code != 0 {
		t.Fatalf("vault list --format text = %d, stderr = %q", code, stderr)
	}
	for _, line := range strings.Split(stdout, "\n") {
		if n := utf8.RuneCountInString(line); n > 40 {
			t.Errorf("line %q is %d runes, want <= 40 (COLUMNS)", line, n)
		}
	}
	found := false
	for _, line := range strings.Split(stdout, "\n") {
		if strings.Contains(line, "LONG_ENV_NAME_FOR_WIDTH_TEST") && strings.HasSuffix(line, "…") {
			found = true
		}
	}
	if !found {
		t.Errorf("stdout = %q, want the over-width row clipped with an ellipsis", stdout)
	}
}

// usageRow mirrors internal/helptext's unexported row/isRow matcher — kept in sync by hand; if
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
	lines := strings.Split(renderHelpPage(vaultPage), "\n")
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

// TestConfigVerbIsUnknown is also the only coverage of Run's unknown-command path (provider lookup → exit 2).
func TestConfigVerbIsUnknown(t *testing.T) {
	useVaultTest(t)
	code, _, stderr := captureOutput(t, func() int { return Run([]string{"claude-code", "config", "show"}) })
	if code != 2 {
		t.Errorf("Run([config show]) = %d, want 2 (unknown verb)", code)
	}
	if !strings.Contains(stderr, `unknown command "config"`) {
		t.Errorf("stderr = %q, want it to name config as unknown", stderr)
	}
}
