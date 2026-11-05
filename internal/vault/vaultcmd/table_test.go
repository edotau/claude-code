package vaultcmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/network/tailscale"
	"github.com/edotau/claude-code/internal/ui/ansi"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// textHasRow reports whether the rendered table has a row for key with exactly value, tolerant of padding.
func textHasRow(stdout, key, value string) bool {
	re := regexp.MustCompile(`(?m)^  ` + regexp.QuoteMeta(key) + `\s\s+` + regexp.QuoteMeta(value) + `$`)
	return re.MatchString(stdout)
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

// TestVaultAgentsTableSpansRecordsToText: agent records fold into one AGENT table once, loose pins stay under PINS.
func TestVaultAgentsTableSpansRecordsToText(t *testing.T) {
	vaulttest.Use(t)
	t.Setenv("COLUMNS", "200")
	vaulttest.Put(t, vault.Pins, "HARNESS_PROVIDER", "anthropic")
	for _, r := range [][3]string{
		{"claude", "profile", "dev"}, {"claude", "model", "sonnet"}, {"claude", "aliases.opus", "opus-x"},
		{"codex", "profile", "personal"}, {"codex", "effort", "high"},
	} {
		vaulttest.SetRecord(t, r[0], r[1], r[2])
	}
	doc := exportDoc(t)
	text := Text(doc, nil, []string{"pins", "agents"}, 200, false)
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
	if !strings.Contains(pinsBlock, "PINS\n") || !textHasRow(pinsBlock, "HARNESS_PROVIDER", "anthropic") {
		t.Errorf("PINS must hold the non-agent pin:\n%s", pinsBlock)
	}
	if colored := Text(doc, nil, []string{"agents"}, 200, true); !strings.Contains(colored, ansi.Dim+"  gemini") {
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

func TestRecordLabelsReportsUnreadableVault(t *testing.T) {
	dir := useVaultTest(t)
	if err := os.MkdirAll(filepath.Join(dir, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state", "vault.db"), []byte("not a vault"), 0o600); err != nil {
		t.Fatal(err)
	}
	var labels map[string]string
	_, _, stderr := captureOutput(t, func() int { labels = recordLabels(); return 0 })
	if len(labels) != 0 || !strings.Contains(stderr, "vault list: reading labels:") {
		t.Fatalf("labels=%v stderr=%q, want empty map and a reading-labels warning", labels, stderr)
	}
}
