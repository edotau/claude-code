package configure

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	agentpkg "github.com/edotau/claude-code/internal/agent"
	_ "github.com/edotau/claude-code/internal/claude" // registers the claude and codex rows ApplyProfile and AgentPins need
	_ "github.com/edotau/claude-code/internal/codex"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// Given codex already on qa and a stale last-used entry for both agents, ApplyProfile moves only claude, keeps the
// model pin, and forgets last-used for all targeted agents.
func TestApplyProfileChangesOnlyDifferingAgents(t *testing.T) {
	vaulttest.UseKeyring(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(providers.EnvSessionProfile, "")
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{
"dev":{"preset":"databricks","host":"https://adb-1.azuredatabricks.net","cliProfile":"dev","auth":{"type":"oauth"}},
"qa":{"preset":"databricks","host":"https://adb-2.azuredatabricks.net","cliProfile":"qa","auth":{"type":"key","command":"echo x"}}}}}`)
	t.Chdir(t.TempDir())
	if err := vault.SetAgentRecord("codex", vault.AgentRecord{Profile: "qa"}); err != nil {
		t.Fatal(err)
	}
	if err := vault.SetAgentRecord("claude", vault.AgentRecord{Profile: "dev", Model: "pinned"}); err != nil {
		t.Fatal(err)
	}
	providers.RecordLastProfile("claude", "stale-claude")
	providers.RecordLastProfile("codex", "stale-codex")
	changed, err := ApplyProfile([]string{"claude", "codex"}, "qa")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(changed, []string{"claude"}) {
		t.Errorf("changed = %v, want [claude]", changed)
	}
	recs := vault.AgentRecords(agentpkg.RosterNames())
	if recs["claude"].Profile != "qa" || recs["claude"].Model != "pinned" || recs["codex"].Profile != "qa" {
		t.Errorf("records = %+v", recs)
	}
	raw, _ := os.ReadFile(filepath.Join(paths.StateDir(), "last-profile.json"))
	if bytes.Contains(raw, []byte("stale-claude")) || bytes.Contains(raw, []byte("stale-codex")) {
		t.Errorf("last-used = %s", raw)
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
	if err := vault.SetAgentRecord("codex", vault.AgentRecord{Profile: "qa", Model: "pinned"}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	assertAgentProfileExclusive(t, &out)
	if err := AgentProfile(&out, "codex", "", false, true); err != nil || out.String() != "codex: off\n" {
		t.Fatalf("--off = %v, %q", err, out.String())
	}
	if rec := vault.GetAgentRecord("codex"); rec.Profile != "" || rec.Model != "pinned" {
		t.Errorf("record after --off = %+v, want empty profile, model kept", rec)
	}
	if err := AgentProfile(&out, "codex", "", true, false); err != nil {
		t.Fatal(err)
	}
	if got := vault.GetAgentRecord("codex").Profile; got != vault.OwnLoginProfiles["codex"] {
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
	if got := vault.GetAgentRecord("codex").Profile; got != "qa" {
		t.Fatalf("profile after rejected calls = %q, want qa", got)
	}
}
