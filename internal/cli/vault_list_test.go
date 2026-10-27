package cli

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

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
				code, stdout, _ := captureOutput(t, func() int { return cmdVault(args) })
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
				if slices.Contains([]string{"SECRETS", "PINS", "ENV", "PROVIDERS", "VENDORS", "SETTINGS", "VAULT"}, line) {
					headings = append(headings, line)
				}
			}
			if !slices.Equal(headings, []string{"SECRETS", "PINS", "ENV", "PROVIDERS", "SETTINGS", "VAULT"}) { // empty VENDORS hidden
				t.Errorf("section order = %v", headings)
			}
			if !strings.HasPrefix(filtered, "SECRETS\n") || !strings.Contains(filtered, "\nPINS\n") {
				t.Error("filtered sections must retain canonical order")
			}
			for _, head := range []string{"ENV", "PROVIDERS", "VENDORS", "SETTINGS", "VAULT"} {
				if strings.Contains(filtered, "\n"+head+"\n") {
					t.Errorf("filtered text includes %s", head)
				}
			}
		})
	}
	for _, names := range [][]string{{"bogus"}, {"all", "bogus"}, {"bogus", "all"}} {
		code, _, stderr := captureOutput(t, func() int { return cmdVault(append([]string{"list"}, names...)) })
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
		"providers":{"fake":{"kind":"openai","auth":{"type":"none"},"models":{"sonnet":"fake-sonnet"}}},
		"agentPlans":{"copilot":[{"tier":"standard","id":"fake-plan"}]},
		"routing":{"opencode":{"ladder":["one","two"]}}}`)
	vaulttest.Put(t, vault.Overlay, "vendors", `{"vendors":{"opencode":{
		"_comment":"omit-this-comment","launch":{"env":{"FAKE_ENV":"enabled"},"args":["--fake"]},
		"future":{"enabled":true}}}}`)
	code, stdout, _ := captureOutput(t, func() int {
		return cmdVault([]string{"list", "providers", "routing", "vendors", "--format", "text"})
	})
	if code != 0 {
		t.Fatalf("list tables exit %d", code)
	}
	normalized := strings.Join(strings.Fields(stdout), " ")
	for _, want := range []string{
		"NAME KIND AUTH OPUS SONNET HAIKU OPENCODE FALLBACK", "fake openai none fake-sonnet",
		"PLANS", "AGENT TIER ID", "copilot standard fake-plan",
		"ROUTING", "opencode.ladder[0] one", "opencode.ladder[1] two",
		"VENDORS opencode", "launch.env.FAKE_ENV enabled", "launch.args --fake", "future.enabled true",
	} {
		if !strings.Contains(normalized, want) {
			t.Errorf("missing table content %q", want)
		}
	}
	if strings.Contains(stdout, "omit-this-comment") || strings.Contains(stdout, "vendors.opencode") {
		t.Error("vendor table retained comment or vendor prefix")
	}
}

// TestVaultProvidersGroupsPresetRows: preset rows collapse into one row listing each env with its credential mark.
func TestVaultProvidersGroupsPresetRows(t *testing.T) {
	old := vaultCredOK
	vaultCredOK = func(name string) bool { return name != "prod" }
	t.Cleanup(func() { vaultCredOK = old })
	raw := `{"providers":{"prod":{"preset":"databricks"},"dev":{"preset":"databricks"},"qa":{"preset":"databricks"}}}`
	got := strings.Join(strings.Fields(vaultProvidersText(json.RawMessage(raw), 200)), " ")
	if !strings.Contains(got, "databricks preset dev✓ qa✓ prod✗") {
		t.Errorf("preset rows not grouped in env order with marks:\n%s", got)
	}
}
