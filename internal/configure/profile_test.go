package configure

import (
	"bytes"
	"reflect"
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

// A second writer replaces an eligible pin just before the transaction: select from that new record.
func TestSwitchProfileSelectsInsideTransaction(t *testing.T) {
	for _, rt := range []route.Route{
		{},
		{Provider: "openai", Profile: "chatgpt"},
		{Provider: "anthropic", Profile: "anthropic"},
		{Provider: "github", Account: "work"},
		{Provider: "databricks", Profile: "auto"},
	} {
		t.Run(rt.Provider+"/"+rt.Profile+rt.Account, func(t *testing.T) {
			vaulttest.Use(t)
			reg := switchTestRegistry()
			initial := vault.AgentRecord{Route: route.Route{Provider: "openai", Profile: "auto"}}
			if err := vault.SetAgentRecord("codex", initial); err != nil {
				t.Fatal(err)
			}
			move, _, _ := SwitchTargets(reg, map[string]vault.AgentRecord{"codex": initial}, "next")
			if !slices.Contains(move, "codex") {
				t.Fatal("negative control: initial record must be eligible")
			}
			newer := vault.AgentRecord{Route: rt, Model: "concurrent-model", Extras: map[string]string{"keep": "new"}}
			calls := 0
			update := func(fn func(map[string]vault.AgentRecord)) error {
				calls++
				if err := vault.SetAgentRecord("codex", newer); err != nil {
					return err
				}
				return vault.UpdateAgentRecords(fn)
			}
			if err := switchProfile(reg, "next", update); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("transactions = %d, want 1", calls)
			}
			if got := vault.GetAgentRecord("codex"); !reflect.DeepEqual(got, newer) {
				t.Fatalf("overwrote concurrent route: %+v, want %+v", got, newer)
			}
			if got := vault.GetAgentRecord("claude").Route; got != (route.Route{Provider: "openai", Profile: "next"}) {
				t.Fatalf("claude = %+v", got)
			}
		})
	}
}

func switchTestRegistry() *providers.Registry {
	return &providers.Registry{Providers: map[string]*providers.Provider{
		"chatgpt": {Name: "chatgpt", Provider: "openai", Auth: providers.Auth{Type: providers.AuthCLI, Login: "codex"}},
		"auto":    {Name: "auto", Provider: "openai", Protocol: providers.ProtocolResponses, BaseURL: "https://example.com", Auth: providers.Auth{Type: providers.AuthKey}},
		"next":    {Name: "next", Provider: "openai", Protocol: providers.ProtocolResponses, BaseURL: "https://example.com", Auth: providers.Auth{Type: providers.AuthKey}},
	}}
}

func TestSwitchNonDatabricksAutoIsFixed(t *testing.T) {
	reg := switchTestRegistry()
	for _, rt := range []route.Route{{Profile: "auto"}, {Provider: "openai", Profile: "auto"}, {Provider: "databricks", Profile: "auto"}} {
		recs := map[string]vault.AgentRecord{"codex": {Route: rt, Model: "keep"}}
		SwitchRecords(reg, recs, "next")
		want := route.Route{Provider: "openai", Profile: "next"}
		if rt.Provider == "databricks" {
			want = rt
		}
		if got := recs["codex"]; got.Route != want || got.Model != "keep" {
			t.Fatalf("from %+v: got %+v, want route %+v", rt, got, want)
		}
	}
}

func TestSwitchUnknownTargetDoesNotMoveOrWrite(t *testing.T) {
	vaulttest.Use(t)
	reg := switchTestRegistry()
	before := vault.AgentRecord{Route: route.Route{Profile: "auto"}, Model: "keep"}
	if err := vault.SetAgentRecord("codex", before); err != nil {
		t.Fatal(err)
	}
	recs := map[string]vault.AgentRecord{"codex": before}
	move, off, kept := SwitchTargets(reg, recs, "missing")
	if len(move) != 0 || !slices.Contains(kept, "codex") || len(off)+len(kept) != len(agentpkg.Roster()) {
		t.Fatalf("move=%v off=%v kept=%v", move, off, kept)
	}
	SwitchRecords(reg, recs, "missing")
	if !reflect.DeepEqual(recs["codex"], before) {
		t.Fatal("unknown target changed preview")
	}
	if err := SwitchProfile(reg, "missing"); err == nil {
		t.Fatal("unknown target accepted")
	}
	if got := vault.GetAgentRecord("codex"); !reflect.DeepEqual(got, before) {
		t.Fatalf("unknown target wrote %+v", got)
	}
}

// Same-provider eligibility is the negative control: removing OwnLogin must move codex and fail this test.
func TestSwitchKeepsSameProviderOwnLogin(t *testing.T) {
	vaulttest.Use(t)
	reg := switchTestRegistry()
	before := vault.AgentRecord{Route: route.Route{Provider: "openai", Profile: "chatgpt"}, Model: "keep", Extras: map[string]string{"keep": "value"}}
	if !agentpkg.CanServe("codex", reg.Providers["next"]) {
		t.Fatal("target must be serve-capable")
	}
	if err := vault.SetAgentRecord("codex", before); err != nil {
		t.Fatal(err)
	}
	move, _, kept := SwitchTargets(reg, map[string]vault.AgentRecord{"codex": before}, "next")
	if slices.Contains(move, "codex") || !slices.Contains(kept, "codex") {
		t.Fatalf("move=%v kept=%v", move, kept)
	}
	if err := SwitchProfile(reg, "next"); err != nil {
		t.Fatal(err)
	}
	if got := vault.GetAgentRecord("codex"); !reflect.DeepEqual(got, before) {
		t.Fatalf("own login changed: %+v", got)
	}
}

func TestSwitchProviderlessTargetDoesNotKeepEmptyRoutes(t *testing.T) {
	reg := &providers.Registry{Providers: map[string]*providers.Provider{"unresolved": {Name: "unresolved"}}}
	move, off, _ := SwitchTargets(reg, nil, "unresolved")
	if !slices.Contains(move, "claude") || !slices.Contains(off, "codex") {
		t.Fatalf("move=%v off=%v", move, off)
	}
	recs := map[string]vault.AgentRecord{}
	SwitchRecords(reg, recs, "unresolved")
	if got := recs["claude"].Route; got != (route.Route{Profile: "unresolved"}) {
		t.Fatalf("claude route = %+v", got)
	}
}
