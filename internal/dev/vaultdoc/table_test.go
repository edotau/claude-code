package vaultdoc

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/cli/ui/ansi"
	_ "github.com/edotau/claude-code/internal/driver/claude" // the vendors register the roster rows the vault tests list
	_ "github.com/edotau/claude-code/internal/driver/codex"
	_ "github.com/edotau/claude-code/internal/driver/copilot"
	_ "github.com/edotau/claude-code/internal/driver/gemini"
	_ "github.com/edotau/claude-code/internal/driver/opencode"
	"github.com/edotau/claude-code/internal/platform/hosttest"
	"github.com/edotau/claude-code/internal/platform/vault/vaulttest"
)

// TestVaultRoutingShowsFallback: the routing section names the effective fallback mode and its source.
func TestVaultRoutingShowsFallback(t *testing.T) {
	t.Setenv("AGENT_ERROR_FALLBACK", "")
	if got := vaultRoutingText(nil, nil, 80); !strings.Contains(got, "none (default)") {
		t.Errorf("unset fallback row missing:\n%s", got)
	}
	if got := vaultRoutingText(nil, map[string]string{"AGENT_ERROR_FALLBACK": "pin"}, 80); !strings.Contains(got, "pin (vault env)") {
		t.Errorf("stored fallback row missing:\n%s", got)
	}
}

// TestVaultProvidersGroupsPresetRows: preset rows collapse into one row listing each env with its credential mark.
func TestVaultProvidersGroupsPresetRows(t *testing.T) {
	old := credOK
	credOK = func(name string) bool { return name != "prod" }
	t.Cleanup(func() { credOK = old })
	raw := `{"providers":{"databricks":{"prod":{"preset":"databricks"},"dev":{"preset":"databricks"},"qa":{"preset":"databricks"}}}}`
	got := strings.Join(strings.Fields(vaultProvidersText(json.RawMessage(raw), 200)), " ")
	if !strings.Contains(got, "databricks databricks ✗prod ✓dev ✓qa") {
		t.Errorf("preset rows not grouped failures-first, then env order, with leading marks:\n%s", got)
	}
}

// TestVaultAgentsTableSpansRecordsToText: agent records fold into one AGENT table once.
func TestVaultAgentsTableSpansRecordsToText(t *testing.T) {
	vaulttest.Use(t)
	t.Setenv("COLUMNS", "200")
	for _, r := range [][3]string{
		{"claude", "profile", "dev"}, {"claude", "model", "sonnet"}, {"claude", "aliases.opus", "opus-x"},
		{"codex", "profile", "copilot"}, {"codex", "effort", "high"},
	} {
		vaulttest.SetRecord(t, r[0], r[1], r[2])
	}
	doc := exportDoc(t)
	text := Text(doc, Records{}, []string{"agents"}, 200, false)
	flat := strings.Join(strings.Fields(text), " ")
	for _, want := range []string{"AGENT PROFILE MODEL EFFORT", "claude dev sonnet opus=opus-x", "codex copilot high", "gemini (off)"} {
		if !strings.Contains(flat, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	if strings.Contains(text, "AGENTS") || strings.Contains(text, "records") || strings.Count(text, "opus-x") != 1 {
		t.Errorf("records repeated outside the table:\n%s", text)
	}
	if colored := Text(doc, Records{}, []string{"agents"}, 200, true); !strings.Contains(colored, ansi.Dim+"  gemini") {
		t.Errorf("off agent with no other fields not dimmed:\n%q", colored)
	}
}

// exportDoc decodes Export()'s object for the current test vault.
func exportDoc(t *testing.T) Doc {
	t.Helper()
	raw, err := Export()
	if err != nil {
		t.Fatal(err)
	}
	var doc Doc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

// TestPresetEnvsSortsByName: env rows sort by name; no label gets a hardcoded rank.
func TestPresetEnvsSortsByName(t *testing.T) {
	old := credOK
	credOK = func(string) bool { return true }
	t.Cleanup(func() { credOK = old })
	if got, want := presetEnvs([]string{"uat", "staging", "qa"}), "✓qa ✓staging ✓uat"; got != want {
		t.Errorf("presetEnvs = %q, want %q", got, want)
	}
}

// TestVaultProviderRowsKeepsSamePresetPerProvider: one preset used under two providers yields one row per provider.
func TestVaultProviderRowsKeepsSamePresetPerProvider(t *testing.T) {
	old := credOK
	credOK = func(string) bool { return true }
	t.Cleanup(func() { credOK = old })
	nested := map[string]json.RawMessage{
		"anthropic":  json.RawMessage(`{"a":{"preset":"x"}}`),
		"databricks": json.RawMessage(`{"b":{"preset":"x"}}`),
	}
	got := map[string]string{}
	for _, r := range vaultProviderRows(nested) {
		got[r[1]] = r[0] + " " + r[2]
	}
	if got["anthropic"] != "x ✓a" || got["databricks"] != "x ✓b" || len(got) != 2 {
		t.Errorf("rows by provider = %v, want anthropic and databricks each with preset x and its own env", got)
	}
}

// TestMain runs in the hermetic host (hosttest): no live provider pins, vault or vendor CLIs are reachable.
func TestMain(m *testing.M) { os.Exit(hosttest.Main(m)) }
