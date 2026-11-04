package vaultcmd

import (
	"bytes"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestVaultListSections(t *testing.T) {
	useVaultTest(t)
	seedVaultListFixture(t)
	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			run := func(names ...string) string {
				t.Helper()
				args := append([]string{"list", "--format", format}, names...)
				code, stdout, _ := captureOutput(t, func() int { return Run(args) })
				if code != 0 {
					t.Fatalf("list sections %v exit %d", names, code)
				}
				if strings.Contains(stdout, "tvly-dev-secret-leak-me-not") {
					t.Fatal("list leaked a raw value")
				}
				return stdout
			}
			bare := run()
			if bare != run("all") {
				t.Error("all differs from bare list")
			}
			filtered := run("PINS", "SeCrEtS", "pins")
			if format == "json" {
				var doc map[string]json.RawMessage
				if err := json.Unmarshal([]byte(filtered), &doc); err != nil {
					t.Fatal(err)
				}
				if len(doc) != 2 || doc["secrets"] == nil || doc["pins"] == nil {
					t.Error("filtered JSON must contain only secrets and pins")
				}
				doc = nil
				if err := json.Unmarshal([]byte(run("pins")), &doc); err != nil {
					t.Fatal(err)
				}
				if _, ok := doc["secrets"]; ok {
					t.Error("pins-only JSON includes secrets")
				}
				return
			}
			var headings []string
			for line := range strings.SplitSeq(bare, "\n") {
				if slices.Contains([]string{"SECRETS", "PINS", "ENV", "PROVIDERS", "SETTINGS", "VAULT"}, line) {
					headings = append(headings, line)
				}
			}
			if !slices.Equal(headings, []string{"SECRETS", "PINS", "ENV", "PROVIDERS", "SETTINGS", "VAULT"}) {
				t.Errorf("section order = %v", headings)
			}
			if !strings.HasPrefix(filtered, "SECRETS\n") || !strings.Contains(filtered, "\nPINS\n") {
				t.Error("filtered sections must retain canonical order")
			}
			for _, head := range []string{"ENV", "PROVIDERS", "SETTINGS", "VAULT"} {
				if strings.Contains(filtered, "\n"+head+"\n") {
					t.Errorf("filtered text includes %s", head)
				}
			}
		})
	}
	for _, names := range [][]string{{"bogus"}, {"all", "bogus"}, {"bogus", "all"}} {
		code, _, stderr := captureOutput(t, func() int { return Run(append([]string{"list"}, names...)) })
		if code != 2 || !strings.Contains(stderr, "bogus") {
			t.Errorf("unknown section %v: exit %d, missing invalid-name hint=%v",
				names, code, !strings.Contains(stderr, "bogus"))
		}
	}
}

func TestVaultListProviderAndVendorTables(t *testing.T) {
	useVaultTest(t)
	t.Setenv("COLUMNS", "200")
	vaulttest.Put(t, vault.Overlay, "providers", `{
		"providers":{"openai":{"fake":{"auth":{"type":"none"},"models":{"sonnet":"fake-sonnet"}}}},
		"agentPlans":{"copilot":[{"tier":"standard","id":"fake-plan"}]},
		"routing":{"opencode":{"ladder":["one","two"]}}}`)
	vaulttest.Put(t, vault.Overlay, "agents", `{"agents":{"opencode":{
		"_comment":"omit-this-comment","launch":{"env":{"FAKE_ENV":"enabled"},"args":["--fake"]},
		"future":{"enabled":true}}}}`)
	code, stdout, _ := captureOutput(t, func() int {
		return Run([]string{"list", "providers", "routing", "agents", "--format", "text"})
	})
	if code != 0 {
		t.Fatalf("list tables exit %d", code)
	}
	normalized := strings.Join(strings.Fields(stdout), " ")
	for _, want := range []string{
		"NAME PROVIDER AUTH SONNET", "fake openai none fake-sonnet",
		"ROUTING", "opencode.ladder[0] one", "opencode.ladder[1] two",
		"launch.env.FAKE_ENV enabled", "launch.args --fake", "future.enabled true",
	} {
		if !strings.Contains(normalized, want) {
			t.Errorf("missing table content %q", want)
		}
	}
	if strings.Contains(stdout, "omit-this-comment") || strings.Contains(stdout, "vendors.opencode") {
		t.Error("vendor table retained comment or vendor prefix")
	}
}

// TestVaultListNestedOverlayRowsPerProfileNoPlans: profiles render one row each under PROVIDER; no KIND, no PLANS.
func TestVaultListNestedOverlayRowsPerProfileNoPlans(t *testing.T) {
	useVaultTest(t)
	t.Setenv("COLUMNS", "200")
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{
		"databricks":{"dev":{"auth":{"type":"none"},"models":{"opus":"o1"}},"qa":{"auth":{"type":"none"}}},
		"local":{"ollama":null}, "github":{"empty":{}}},
		"agentPlans":{"copilot":[{"tier":"standard","id":"fake-plan"}]}}`)
	code, stdout, _ := captureOutput(t, func() int { return Run([]string{"list", "providers", "--format", "text"}) })
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	got := strings.Join(strings.Fields(stdout), " ")
	for _, want := range []string{"NAME PROVIDER AUTH OPUS", "dev databricks none o1", "qa databricks none", "ollama local (removed)"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "PLANS") || strings.Contains(stdout, "KIND") || strings.Contains(stdout, "SONNET") {
		t.Errorf("PLANS, KIND and all-empty columns must be hidden:\n%s", stdout)
	}
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"local":{"ollama":null},"github":{"empty":{}}}}`)
	_, stdout, _ = captureOutput(t, func() int { return Run([]string{"list", "--format", "text"}) })
	if strings.Contains(stdout, "PROVIDERS") {
		t.Errorf("a section with only empty/removed rows must not render:\n%s", stdout)
	}
}

// A secret written by vault.Store (what `vault set` uses) shows in `vault list` masked, with its length.
func TestVaultListShowsStoredSecretMasked(t *testing.T) {
	useVaultTest(t)
	const val = "abcdefghijklmnopqrstuvwxyz0123"
	if err := vault.Store("ZZ_STORE_SECRET", val); err != nil {
		t.Fatal(err)
	}
	code, out, _ := captureOutput(t, func() int { return Run([]string{"list", "secrets", "--format", "json"}) })
	if code != 0 || strings.Contains(out, val) {
		t.Fatalf("vault list: %d (raw value leaked: %v)", code, strings.Contains(out, val))
	}
	var doc struct {
		Secrets []struct {
			Name, Masked string
			Length       int
		} `json:"secrets"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("vault list output not JSON: %v\n%s", err, out)
	}
	for _, e := range doc.Secrets {
		if e.Name == "ZZ_STORE_SECRET" {
			if e.Masked == "" || e.Length != len(val) {
				t.Errorf("entry = %+v, want a mask and length %d", e, len(val))
			}
			return
		}
	}
	t.Fatalf("ZZ_STORE_SECRET missing from vault list: %s", out)
}

// Captured output defaults to JSON with masked secrets alongside configure.Export's sections.
func TestVaultListDefaultIsJSONUnderCaptureOutput(t *testing.T) {
	useVaultTest(t)
	seedVaultListFixture(t)
	want, err := configure.Export()
	if err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := captureOutput(t, func() int { return Run([]string{"list"}) })
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
	code, stdout, stderr := captureOutput(t, func() int { return Run([]string{"list", "--format", "json"}) })
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
		stored, err := vault.ListStored(nil)
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
	code, _, stderr := captureOutput(t, func() int { return Run([]string{"list", "--format", "bogus"}) })
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
	code, stdout, stderr := captureOutput(t, func() int { return Run([]string{"list", "--format", "text"}) })
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
	code, stdout, stderr := captureOutput(t, func() int { return Run([]string{"list", "--format", "text"}) })
	if code != 0 || !strings.HasPrefix(stdout, "  AGENT\n  claude\n") {
		t.Fatalf("bare vault list = %d %q (stderr %q), want 0 and only the AGENT table", code, stdout, stderr)
	}
	named := append([]string{"list"}, Sections...)
	code, stdout, stderr = captureOutput(t, func() int { return Run(append(named[:len(named)-1], "--format", "text")) })
	if code != 0 {
		t.Fatalf("vault list <sections> = %d, stderr = %q", code, stderr)
	}
	for _, head := range []string{"SECRETS", "PINS", "ENV", "PROVIDERS", "SETTINGS"} {
		if !strings.Contains(stdout, head+"\n  (none)\n") {
			t.Errorf("stdout = %q, want %s rendered as (none)", stdout, head)
		}
	}
	code, stdout, _ = captureOutput(t, func() int { return Run([]string{"list", "vault", "--format", "text"}) })
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
	code, stdout, stderr := captureOutput(t, func() int { return Run([]string{"list", "--format", "text"}) })
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
