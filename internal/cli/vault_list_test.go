package cli

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/ui/ansi"
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
		"providers":{"openai":{"fake":{"auth":{"type":"none"},"models":{"sonnet":"fake-sonnet"}}}},
		"agentPlans":{"copilot":[{"tier":"standard","id":"fake-plan"}]},
		"routing":{"opencode":{"ladder":["one","two"]}}}`)
	vaulttest.Put(t, vault.Overlay, "agents", `{"agents":{"opencode":{
		"_comment":"omit-this-comment","launch":{"env":{"FAKE_ENV":"enabled"},"args":["--fake"]},
		"future":{"enabled":true}}}}`)
	code, stdout, _ := captureOutput(t, func() int {
		return cmdVault([]string{"list", "providers", "routing", "agents", "--format", "text"})
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

// TestVaultProvidersGroupsPresetRows: preset rows collapse into one row listing each env with its credential mark.
func TestVaultProvidersGroupsPresetRows(t *testing.T) {
	old := vaultCredOK
	vaultCredOK = func(name string) bool { return name != "prod" }
	t.Cleanup(func() { vaultCredOK = old })
	raw := `{"providers":{"databricks":{"prod":{"preset":"databricks"},"dev":{"preset":"databricks"},"qa":{"preset":"databricks"}}}}`
	got := strings.Join(strings.Fields(vaultProvidersText(json.RawMessage(raw), 200)), " ")
	if !strings.Contains(got, "databricks databricks ✗prod ✓dev ✓qa") {
		t.Errorf("preset rows not grouped failures-first, then env order, with leading marks:\n%s", got)
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
	code, stdout, _ := captureOutput(t, func() int { return cmdVault([]string{"list", "providers", "--format", "text"}) })
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
	_, stdout, _ = captureOutput(t, func() int { return cmdVault([]string{"list", "--format", "text"}) })
	if strings.Contains(stdout, "PROVIDERS") {
		t.Errorf("a section with only empty/removed rows must not render:\n%s", stdout)
	}
}

// TestVaultAgentsTableSpansRecordsToText: agent records fold into one AGENT table once, loose pins stay under PINS.
func TestVaultAgentsTableSpansRecordsToText(t *testing.T) {
	useVaultTest(t)
	t.Setenv("COLUMNS", "200")
	vaulttest.Put(t, vault.Pins, "HARNESS_PROVIDER", "anthropic")
	for _, r := range [][3]string{
		{"claude", "profile", "dev"}, {"claude", "model", "sonnet"}, {"claude", "slots.opus", "opus-x"},
		{"codex", "profile", "personal"}, {"codex", "effort", "high"},
	} {
		vaulttest.SetRecord(t, r[0], r[1], r[2])
	}
	raw, err := configure.Export()
	if err != nil {
		t.Fatal(err)
	}
	var doc configure.Doc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	text := renderVaultTextStyled(doc, nil, []string{"pins", "agents"}, 200, false)
	flat := strings.Join(strings.Fields(text), " ")
	for _, want := range []string{"AGENT PROFILE MODEL EFFORT", "claude dev sonnet opus=opus-x", "codex personal high", "gemini (off)"} {
		if !strings.Contains(flat, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	if strings.Contains(text, "AGENTS") || strings.Contains(text, "records") || strings.Count(text, "opus-x") != 1 {
		t.Errorf("records repeated outside the table:\n%s", text)
	}
	pinsBlock, _, _ := strings.Cut(text, "AGENT")
	if !strings.Contains(pinsBlock, "PINS\n") || !vaultTextHasRow(pinsBlock, "HARNESS_PROVIDER", "anthropic") {
		t.Errorf("PINS must hold the non-agent pin:\n%s", pinsBlock)
	}
	if colored := renderVaultTextStyled(doc, nil, []string{"agents"}, 200, true); !strings.Contains(colored, ansi.Dim+"  gemini") {
		t.Errorf("off agent with no other fields not dimmed:\n%q", colored)
	}
}
