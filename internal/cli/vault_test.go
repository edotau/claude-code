package cli

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/secretstore"
	uivault "github.com/edotau/claude-code/internal/ui/vault"
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
			useVaultTest(t)
			if got := cmdVault(tt.args); got != tt.want {
				t.Errorf("cmdVault(%q) = %d, want %d", tt.args, got, tt.want)
			}
		})
	}
}

// vault edit has no flags or commands of its own; -h must print its own usage-only page, not vaultPage's
// COMMANDS list (every other vault -h path already covers the parent page itself).
func TestCmdVaultEditHelpPrintsOwnPage(t *testing.T) {
	useVaultTest(t)
	code, stdout, stderr := captureOutput(t, func() int { return cmdVault([]string{"edit", "-h"}) })
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
	if err := secretstore.Store("TAILSCALE_API_KEY", value); err != nil {
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
		value, source, ok := secretstore.Get(name)
		if !ok || secretstore.Redact(value) != secretstore.Redact(want) || source != "vault" {
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
	value, source, ok := secretstore.Get("TAILSCALE_API_KEY")
	if !ok || secretstore.Redact(value) != secretstore.Redact("apikeyvalue") || source != "vault" {
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
			names := secretstore.Names()
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
	names := secretstore.Names()
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
// the second of a pair — secretstore.Store would reject it anyway, but half a pair must never land.
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
			names := secretstore.Names()
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

// Captured output defaults to JSON with masked secrets alongside configure.Export's sections.
func TestVaultListDefaultIsJSONUnderCaptureOutput(t *testing.T) {
	useVaultTest(t)
	seedVaultListFixture(t)
	want, err := configure.Export()
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
	want, err := configure.Export()
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
	// "routing" is derived from the providers overlay's nested routing object, not one of configure.Export's
	// own top-level sections; just check it decodes, then drop it before the section-by-section compare.
	if r, ok := got["routing"]; ok {
		var routing providers.Routing
		if err := json.Unmarshal(r, &routing); err != nil {
			t.Fatalf("routing section: %v", err)
		}
		delete(got, "routing")
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
		t.Fatal("list JSON changed configure.Export sections")
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
		`{"default":"anthropic","fallback":["one","two"],"providers":{"openai":null,"anthropic":{"custom":{"models":{"opus":"m1"}}}}}`)
	vaulttest.Put(t, vault.Overlay, "agents", `{}`)
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
	for _, head := range []string{"PINS", "ENV", "PROVIDERS", "SETTINGS", "VAULT"} {
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
	if !strings.Contains(stdout, "AGENT\n  claude\n") || strings.Contains(stdout, "AGENTS") {
		t.Errorf("stdout = %q, want an AGENT table with no AGENTS heading", stdout)
	}
	for _, row := range []string{
		"NAME PROVIDER AUTH OPUS",
		"custom anthropic m1", "openai openai (removed)",
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

// A bare list hides empty sections; named sections still render (none), including absent overlay documents.
func TestVaultListTextEmptyVaultAllSectionsNone(t *testing.T) {
	useVaultTest(t)
	code, stdout, stderr := captureOutput(t, func() int { return cmdVault([]string{"list", "--format", "text"}) })
	if code != 0 || !strings.HasPrefix(stdout, "  AGENT\n  claude\n") {
		t.Fatalf("bare vault list = %d %q (stderr %q), want 0 and only the AGENT table", code, stdout, stderr)
	}
	named := append([]string{"list"}, uivault.Sections...)
	code, stdout, stderr = captureOutput(t, func() int { return cmdVault(append(named[:len(named)-1], "--format", "text")) })
	if code != 0 {
		t.Fatalf("vault list <sections> = %d, stderr = %q", code, stderr)
	}
	for _, head := range []string{"SECRETS", "PINS", "ENV", "PROVIDERS", "SETTINGS"} {
		if !strings.Contains(stdout, head+"\n  (none)\n") {
			t.Errorf("stdout = %q, want %s rendered as (none)", stdout, head)
		}
	}
	code, stdout, _ = captureOutput(t, func() int { return cmdVault([]string{"list", "vault", "--format", "text"}) })
	if code != 0 || stdout != "VAULT\n  (none)\n" {
		t.Errorf("vault list vault = %d %q, want VAULT rendered as (none) when key_command is unset", code, stdout)
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
	for line := range strings.SplitSeq(stdout, "\n") {
		if n := utf8.RuneCountInString(line); n > 40 {
			t.Errorf("line %q is %d runes, want <= 40 (COLUMNS)", line, n)
		}
	}
	found := false
	for line := range strings.SplitSeq(stdout, "\n") {
		if strings.Contains(line, "LONG_ENV_NAM…_WIDTH_TEST") && strings.HasSuffix(line, "…") {
			found = true
		}
	}
	if !found {
		t.Errorf("stdout = %q, want the over-width row clipped with an ellipsis", stdout)
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

func useVaultTest(t *testing.T) string {
	t.Helper()
	dir := vaulttest.Use(t)
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARNESS_VAULT_KEY", base64.StdEncoding.EncodeToString(key))
	return dir
}

func TestVaultSetCopilotTokenRefusesShortValue(t *testing.T) {
	useVaultTest(t)
	withStdin(t, "ok\n")

	code, _, stderr := captureOutput(t, func() int { return cmdVault([]string{"set", "COPILOT_GITHUB_TOKEN"}) })
	if code == 0 {
		t.Fatal("set COPILOT_GITHUB_TOKEN accepted a short value")
	}
	if !strings.Contains(stderr, "not a GitHub token") {
		t.Fatalf("stderr = %q, want the not-a-GitHub-token refusal", stderr)
	}
	if _, _, ok := secretstore.Get("COPILOT_GITHUB_TOKEN"); ok {
		t.Fatal("a refused value must not be stored")
	}
}

func TestVaultSetTailscaleCredentialRefusesMalformedShape(t *testing.T) {
	useVaultTest(t)
	withStdin(t, "not-a-client-id\n")

	code, _, stderr := captureOutput(t, func() int { return cmdVault([]string{"set", "TAILSCALE_OAUTH_CLIENT_ID"}) })
	if code == 0 {
		t.Fatal("set TAILSCALE_OAUTH_CLIENT_ID accepted a malformed value")
	}
	if !strings.Contains(stderr, "want a client ID") {
		t.Fatalf("stderr = %q, want the client-ID shape refusal", stderr)
	}
	if _, _, ok := secretstore.Get("TAILSCALE_OAUTH_CLIENT_ID"); ok {
		t.Fatal("a refused value must not be stored")
	}
}

// A valid-shaped GitHub token is accepted and sealed in the vault.
func TestVaultSetCopilotTokenAcceptsValidShape(t *testing.T) {
	useVaultTest(t)
	const tok = "gh" + "p_validshapedFAKEtoken1234567890"
	withStdin(t, tok+"\n")

	code, stdout, stderr := captureOutput(t, func() int { return cmdVault([]string{"set", "COPILOT_GITHUB_TOKEN"}) })
	if code != 0 {
		t.Fatalf("set COPILOT_GITHUB_TOKEN exit = %d", code)
	}
	if v, _, ok := secretstore.Get("COPILOT_GITHUB_TOKEN"); !ok || secretstore.Redact(v) != secretstore.Redact(tok) {
		t.Fatal("COPILOT_GITHUB_TOKEN was not stored correctly")
	}
	if strings.Contains(stdout+stderr, tok) {
		t.Fatal("set leaked the raw value")
	}
	if !strings.Contains(stderr, secretstore.Redact(tok)) {
		t.Fatal("set did not report the masked value")
	}
}

func TestVaultSetNameDryRunStoresNothing(t *testing.T) {
	useVaultTest(t)
	const value = "obviously-fake-dry-run"
	withStdin(t, value+"\n")
	code, stdout, stderr := captureOutput(t, func() int {
		return cmdVault([]string{"set", "VAULT_TEST_DUMMY", "--dry-run"})
	})
	if code != 0 || !strings.Contains(stdout, "would add VAULT_TEST_DUMMY") {
		t.Fatalf("set --dry-run: exit %d, expected preview=%v",
			code, strings.Contains(stdout, "would add VAULT_TEST_DUMMY"))
	}
	if strings.Contains(stdout+stderr, value) {
		t.Fatal("dry run leaked the raw value")
	}
	if len(secretstore.Names()) != 0 {
		t.Fatal("dry run stored a secret")
	}
}

func TestVaultSetNameAndTailscaleRefused(t *testing.T) {
	useVaultTest(t)
	withStdin(t, "obviously-fake-unused\n")
	code, _, _ := captureOutput(t, func() int {
		return cmdVault([]string{"set", "VAULT_TEST_DUMMY", "--tailscale"})
	})
	if code != 2 || len(secretstore.Names()) != 0 {
		t.Fatalf("set NAME --tailscale: exit %d, stored names=%v", code, secretstore.Names())
	}
}

func TestVaultDeleteStoredSecret(t *testing.T) {
	useVaultTest(t)
	const value = "obviously-fake-delete"
	if err := secretstore.Store("VAULT_TEST_DUMMY", value); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := captureOutput(t, func() int {
		return cmdVault([]string{"delete", "VAULT_TEST_DUMMY"})
	})
	if code != 0 || len(secretstore.Names()) != 0 {
		t.Fatalf("delete: exit %d, stored names=%v", code, secretstore.Names())
	}
	if strings.Contains(stdout+stderr, value) {
		t.Fatal("delete leaked the raw value")
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

func TestVaultExportRejectsStdout(t *testing.T) {
	useVaultTest(t)
	if err := secretstore.Store("ALPHA", "obviously-fake-alpha"); err != nil {
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
			if err := secretstore.Store("ALPHA", value); err != nil {
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
			if secretstore.Redact(pairs[0].Value) != secretstore.Redact(value) {
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
	if v, _, _ := secretstore.Lookup("GAMMA"); v != "" {
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
		got, source, err := secretstore.Lookup(name)
		if err != nil || source != "vault" || secretstore.Redact(got) != secretstore.Redact(value) {
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
	if v, _, _ := secretstore.Lookup("GAMMA"); v != "" {
		t.Fatal("GAMMA stored despite a malformed sibling line")
	}
}

func TestVaultGetMultipleNames(t *testing.T) {
	for _, export := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "export"}[export], func(t *testing.T) {
			useVaultTest(t)
			values := []string{"obviously-fake-alpha", "obviously-fake-beta's-value"}
			for i, name := range []string{"ALPHA", "BETA"} {
				if err := secretstore.Store(name, values[i]); err != nil {
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
				stdout = strings.ReplaceAll(stdout, value, secretstore.Redact(values[i]))
				prefix := ""
				if export {
					prefix = "export "
				}
				wantSb151.WriteString(prefix)
				wantSb151.WriteString(name)
				wantSb151.WriteByte('=')
				wantSb151.WriteString(secretstore.Redact(values[i]))
				wantSb151.WriteByte('\n')
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
	if err := secretstore.Store("ALPHA", "obviously-fake-alpha"); err != nil {
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

func TestVaultImportRejectsBadGitHubToken(t *testing.T) {
	useVaultTest(t)
	withStdin(t, "GITHUB_WORK_PAT=not-a-token\n")
	code, _, stderr := captureOutput(t, func() int { return cmdVault([]string{"import", "-", "--yes"}) })
	if code == 0 || !strings.Contains(stderr, "not a GitHub token") {
		t.Fatalf("exit %d, stderr=%q, want a GitHub-token rejection", code, stderr)
	}
	if v, _, _ := secretstore.Lookup("GITHUB_WORK_PAT"); v != "" {
		t.Fatal("bad GitHub token stored by import")
	}
}
