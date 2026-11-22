package configure

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	agentpkg "github.com/edotau/claude-code/internal/agent"
	_ "github.com/edotau/claude-code/internal/claude" // registers the claude and codex rows ApplyProfile and AgentPins need
	_ "github.com/edotau/claude-code/internal/codex"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// Given codex already on qa, ApplyProfile moves only claude and keeps its model pin.
func TestApplyProfileChangesOnlyDifferingAgents(t *testing.T) {
	vaulttest.UseKeyring(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(providers.EnvSessionProfile, "")
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{
"dev":{"preset":"databricks","host":"https://adb-1.azuredatabricks.net","cliProfile":"dev","auth":{"type":"oauth"}},
"qa":{"preset":"databricks","host":"https://adb-2.azuredatabricks.net","cliProfile":"qa","auth":{"type":"key","command":"echo x"}}}}}`)
	t.Chdir(t.TempDir())
	if err := vault.SetAgentRecord("codex", vault.AgentRecord{Route: route.Route{Profile: "qa"}}); err != nil {
		t.Fatal(err)
	}
	if err := vault.SetAgentRecord("claude", vault.AgentRecord{Model: "pinned", Route: route.Route{Profile: "dev"}}); err != nil {
		t.Fatal(err)
	}
	changed, err := ApplyProfile([]string{"claude", "codex"}, "qa")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(changed, []string{"claude"}) {
		t.Errorf("changed = %v, want [claude]", changed)
	}
	recs := vault.AgentRecords(agentpkg.RosterNames())
	if recs["claude"].Route.Profile != "qa" || recs["claude"].Model != "pinned" || recs["codex"].Route.Profile != "qa" {
		t.Errorf("records = %+v", recs)
	}
}

// Given codex on qa, AgentProfile --off clears its record and prints "off", --clear returns it to its own login, a
// conflicting flag mix errors without writing, and claude refuses --off.
func TestAgentProfileOffClearExclusive(t *testing.T) {
	vaulttest.UseKeyring(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(providers.EnvSessionProfile, "")
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{
"qa":{"preset":"databricks","host":"https://adb-2.azuredatabricks.net","cliProfile":"qa","auth":{"type":"key","command":"echo x"}}}}}`)
	t.Chdir(t.TempDir())
	if err := vault.SetAgentRecord("codex", vault.AgentRecord{Model: "pinned", Route: route.Route{Profile: "qa"}}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	assertAgentProfileExclusive(t, &out)
	if err := AgentProfile(&out, "codex", "", false, true); err != nil || out.String() != "codex: off\n" {
		t.Fatalf("--off = %v, %q", err, out.String())
	}
	if rec := vault.GetAgentRecord("codex"); rec.Route.Profile != "" || rec.Model != "pinned" {
		t.Errorf("record after --off = %+v, want empty profile, model kept", rec)
	}
	if err := AgentProfile(&out, "codex", "", true, false); err != nil {
		t.Fatal(err)
	}
	if got := vault.GetAgentRecord("codex").Route.Profile; got != vault.OwnLoginProfiles["codex"] {
		t.Errorf("profile after --clear = %q, want own login %q", got, vault.OwnLoginProfiles["codex"])
	}
	if err := AgentProfile(&out, "claude", "", false, true); err == nil || !strings.Contains(err.Error(), "always on") {
		t.Errorf("claude --off = %v, want always on", err)
	}
}

// assertAgentProfileExclusive checks that combined --clear/--off/profile calls are rejected and leave the pin alone.
func assertAgentProfileExclusive(t *testing.T, out *bytes.Buffer) {
	t.Helper()
	for _, c := range []struct {
		profile    string
		clear, off bool
	}{{"", true, true}, {"qa", false, true}, {"qa", true, false}} {
		if err := AgentProfile(out, "codex", c.profile, c.clear, c.off); err == nil {
			t.Errorf("AgentProfile(%q, clear=%v, off=%v) = nil, want exclusive error", c.profile, c.clear, c.off)
		}
	}
	if got := vault.GetAgentRecord("codex").Route.Profile; got != "qa" {
		t.Fatalf("profile after rejected calls = %q, want qa", got)
	}
}
