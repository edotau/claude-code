package client

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// TestListAgentsRegistryLoadErrorNoPanic proves the roster survives a providers.Load() failure and no longer
// lists the folded-in "api" agent.
func TestListAgentsRegistryLoadErrorNoPanic(t *testing.T) {
	vaulttest.Use(t)
	t.Setenv(providers.EnvSessionProfile, "")
	vaulttest.Put(t, vault.Overlay, "providers", "{not valid json")
	if _, err := providers.Load(); err == nil {
		t.Fatal("providers.Load() must fail on malformed overlay JSON for this test to prove anything")
	}

	var lines []string
	for _, r := range RosterRows() { // must not panic despite a nil registry from the failed Load()
		lines = append(lines, r.Line())
	}
	out := strings.Join(lines, "\n")
	if !strings.Contains(out, " claude ") {
		t.Errorf("no claude row in output:\n%s", out)
	}
	for _, line := range lines {
		if f := strings.Fields(line); len(f) >= 2 && f[1] == "api" {
			t.Errorf("api agent still listed: %q", line)
		}
	}
}

type capabilitySchemaRow struct {
	Name, Binary string
	Caps         map[string]string
	Events       []string
}

func assertCapabilitySchemaRow(t *testing.T, r capabilitySchemaRow) {
	t.Helper()
	for _, k := range []string{"cost", "background", "live", "remote", "env", "system-prompt"} {
		if r.Caps[k] == "" {
			t.Errorf("%s: caps lacks %s", r.Name, k)
		}
	}
	if r.Binary == "" || !slices.Contains(r.Events, "start") || !slices.Contains(r.Events, "result") {
		t.Errorf("%s: binary %q events %v", r.Name, r.Binary, r.Events)
	}
}

// agents --format json carries each agent's binary, the cost/background/live/remote levels and its event kinds.
func TestAgentsJSONCapabilitySchema(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	out, err := json.Marshal(RosterRows())
	if err != nil {
		t.Fatal(err)
	}
	var rows []capabilitySchemaRow
	if err := json.Unmarshal(out, &rows); err != nil {
		t.Fatal(err, string(out))
	}
	byName := map[string]int{}
	for i, r := range rows {
		byName[r.Name] = i
		assertCapabilitySchemaRow(t, r)
	}
	if c := rows[byName["claude"]]; c.Caps["live"] != "native" || c.Caps["cost"] != "native" {
		t.Errorf("claude caps = %v, want live and cost native", c.Caps)
	}
	if g := rows[byName["gemini"]]; g.Binary != "agy" || g.Caps["live"] != "native" {
		t.Errorf("gemini = %+v, want binary agy and live native", g)
	}
}
