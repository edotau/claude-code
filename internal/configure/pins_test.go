package configure

import (
	"slices"
	"strings"
	"testing"
	"time"

	agentpkg "github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// --provider equal to the session's own subscribed provider stores the bare spec, unqualified.
func TestApplyModelPinsBareWhenProviderMatchesSession(t *testing.T) {
	hosttest.PinScratch(t)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	vaulttest.SetRecord(t, "claude", "profile", "copilot")
	if code, handled := ApplyModelPins(reg, "copilot", false, []string{"opus=claude-opus-5.5"}, nil); handled || code != 0 {
		t.Fatalf("ApplyModelPins: code=%d handled=%v, want 0, false", code, handled)
	}
	if got := providers.AgentRecord("claude").Aliases["opus"]; got != "claude-opus-5.5" {
		t.Errorf("CLAUDE_OPUS_MODEL = %q, want the bare spec %q", got, "claude-opus-5.5")
	}
}

// A pin for a profile other than the session's is refused: every alias resolves on the session profile (D5).
func TestApplyModelPinsRefusesOtherProfile(t *testing.T) {
	hosttest.PinScratch(t)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	vaulttest.SetRecord(t, "claude", "profile", "anthropic")
	if code, handled := ApplyModelPins(reg, "copilot", false, []string{"all=claude-opus-5.5"}, nil); !handled || code == 0 {
		t.Fatalf("ApplyModelPins: code=%d handled=%v, want the refusal", code, handled)
	}
	if got := providers.AgentRecord("claude").Model; got != "" {
		t.Errorf("CLAUDE_MODEL = %q, want nothing written", got)
	}
}

func TestSetAgentPinClearReportsUnreadableVault(t *testing.T) {
	vaulttest.Use(t)
	const corrupt = `{"claude":{"model":`
	vaulttest.Put(t, vault.Overlay, vault.AgentRecordsKey, corrupt)
	for _, field := range []string{"MODEL", "PROFILE"} {
		t.Run(field, func(t *testing.T) {
			changed, err := SetAgentPin("claude", field, "")
			if err == nil || !strings.Contains(err.Error(), "agent-records is corrupt") {
				t.Fatalf("clearing %s = (%v, %v), want corrupt-vault error", field, changed, err)
			}
			if changed {
				t.Fatalf("clearing %s reported a change despite unreadable vault", field)
			}
		})
	}
	if err := vault.View(func(tx *vault.Tx) error {
		if got, _ := tx.Get(vault.Overlay, vault.AgentRecordsKey); got != corrupt {
			t.Errorf("failed clear changed vault contents to %q", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// A record invalid in an untouched field does not block an edit of another field, and its report is unchanged.
func TestSetAgentPinIgnoresInvalidUntouchedFields(t *testing.T) {
	hosttest.PinScratch(t)
	bad := vault.AgentRecord{Launch: &vault.AgentLaunch{Env: []string{"NO_EQUALS_SIGN"}}}
	wantReport := agentpkg.ValidateRecord("claude", bad)
	if wantReport == nil || !strings.Contains(wantReport.Error(), "launch.env") {
		t.Fatalf("setup: the record should fail on launch.env, got %v", wantReport)
	}
	for _, c := range []struct{ field, val string }{{"MODEL", "pinned"}, {"PROFILE", "anthropic"}} {
		t.Run(c.field, func(t *testing.T) {
			if err := vault.SetAgentRecord("claude", bad); err != nil {
				t.Fatal(err)
			}
			if changed, err := SetAgentPin("claude", c.field, c.val); err != nil || !changed {
				t.Fatalf("SetAgentPin(%s) = (%v, %v), want applied", c.field, changed, err)
			}
			got := vault.GetAgentRecord("claude")
			if err := agentpkg.ValidateRecord("claude", got); err == nil || err.Error() != wantReport.Error() {
				t.Errorf("stored record report = %v, want %v", err, wantReport)
			}
		})
	}
}

// An edit that makes the touched field invalid is refused before the vault is written, whatever else is stored.
func TestSetAgentPinRefusesInvalidTouchedFieldBeforeStoring(t *testing.T) {
	hosttest.PinScratch(t)
	if err := vault.SetAgentRecord("claude", vault.AgentRecord{Model: "kept"}); err != nil {
		t.Fatal(err)
	}
	stored := func() string {
		var raw string
		if err := vault.View(func(tx *vault.Tx) error { raw, _ = tx.Get(vault.Overlay, vault.AgentRecordsKey); return nil }); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	before := stored()
	changed, err := SetAgentPin("claude", "EFFORT", "no-such-effort")
	if err == nil || !strings.Contains(err.Error(), "effort") || changed {
		t.Fatalf("SetAgentPin(EFFORT) = (%v, %v), want an effort refusal", changed, err)
	}
	if after := stored(); after != before {
		t.Errorf("refused edit changed the stored records:\n before %s\n after  %s", before, after)
	}
}

// Claude's profile picker lists the registry rows it can run on, never another agent's sign-in.
func TestSeedProfilesClaudeListsServableRows(t *testing.T) {
	reg := &providers.Registry{Providers: map[string]*providers.Provider{
		"dev":     {Name: "dev", Preset: providers.PresetDatabricks},
		"chatgpt": {Name: "chatgpt", Auth: providers.Auth{Type: providers.AuthCLI, Login: "codex"}},
	}}
	if got := SeedProfiles(reg, "claude"); !slices.Equal(got, []string{"dev"}) {
		t.Fatalf("SeedProfiles(claude) = %v, want [dev]", got)
	}
}

// FALLBACK writes the record's fallback profile (not an alias) and refuses an unknown profile before any write.
func TestSetAgentPinFallback(t *testing.T) {
	hosttest.PinScratch(t)
	if changed, err := SetAgentPin("claude", "FALLBACK", "anthropic"); err != nil || !changed {
		t.Fatalf("SetAgentPin(FALLBACK) = (%v, %v), want applied", changed, err)
	}
	if rec := vault.GetAgentRecord("claude"); rec.Fallback != "anthropic" || len(rec.Aliases) != 0 {
		t.Errorf("record = %+v, want fallback anthropic and no alias", rec)
	}
	if ev, _ := audit.Read(time.Time{}); slices.ContainsFunc(ev, func(e audit.Event) bool { return e.Action == audit.ModelPin }) {
		t.Error("a fallback change is not a model pin, yet it was audited as one")
	}
	if _, err := SetAgentPin("claude", "FALLBACK", "nosuch"); err == nil {
		t.Error("an unknown fallback profile was accepted")
	}
	if got := vault.GetAgentRecord("claude").Fallback; got != "anthropic" {
		t.Errorf("a refused write changed the fallback to %q", got)
	}
}

// A model pin written through SetAgentPin leaves one audit.ModelPin line naming the field and the new value.
func TestSetAgentPinAuditsModelPin(t *testing.T) {
	hosttest.PinScratch(t)
	if changed, err := SetAgentPin("claude", "OPUS_MODEL", "claude-opus-5-5"); err != nil || !changed {
		t.Fatalf("SetAgentPin(OPUS_MODEL) = (%v, %v), want applied", changed, err)
	}
	ev, err := audit.Read(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range ev {
		if e.Action == audit.ModelPin && e.Resource == "claude.aliases.opus" && e.Detail == "claude-opus-5-5" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("model-pin audit events for claude.aliases.opus = %d, want 1 (all: %+v)", n, ev)
	}
}

// A model no row serves is refused before any write; auto always passes.
func TestValidateAgentModelRefusesUnknown(t *testing.T) {
	hosttest.PinScratch(t)
	if err := ValidateAgentModel("codex", "", "bogus:opus"); err == nil {
		t.Error("bogus-model-xyz accepted, want a refusal")
	}
	if err := ValidateAgentModel("codex", "", "auto"); err != nil {
		t.Errorf("auto refused: %v", err)
	}
}

// A pin is judged, and the refusal names the profile, on the session's profile before the claude record's.
func TestValidateClaudePinsJudgesOnSessionProfile(t *testing.T) {
	hosttest.PinScratch(t)
	row := map[string]any{"preset": "databricks", "host": "https://example.cloud.databricks.com", "auth": map[string]any{"type": "pat"}}
	if err := providers.SetProviderFields("qa", row); err != nil {
		t.Fatal(err)
	}
	if err := router.SaveServed(router.Served{Profile: "qa", FetchedAt: time.Now(),
		Models: []router.ServedModel{{ID: "databricks-claude-opus-5-5", Ready: "READY"}}}); err != nil {
		t.Fatal(err)
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	vaulttest.SetRecord(t, "claude", "profile", "anthropic")
	t.Setenv(providers.EnvSessionProfile, "qa")
	err = validateClaudePins(reg, "", map[string]string{"opus": "bogus-model-xyz"})
	if err == nil || !strings.Contains(err.Error(), "profile qa") {
		t.Errorf("err = %v, want a refusal naming profile qa", err)
	}
}
