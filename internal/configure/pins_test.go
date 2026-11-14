package configure

import (
	"strings"
	"testing"

	agentpkg "github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// ApplyModelPins qualifies a bare model spec with --provider when it differs from the session's
// subscribed provider, so the pin resolves against the provider it was validated for.
func TestApplyModelPinsQualifiesAcrossSessionProvider(t *testing.T) {
	hosttest.PinScratch(t)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	vaulttest.SetRecord(t, "claude", "profile", "anthropic")
	if code, handled := ApplyModelPins(reg, "personal", false, []string{"opus=claude-opus-5.5"}, nil); handled || code != 0 {
		t.Fatalf("ApplyModelPins: code=%d handled=%v, want 0, false", code, handled)
	}
	if got := providers.AgentRecord("claude").Aliases["opus"]; got != "personal:claude-opus-5.5" {
		t.Errorf("CLAUDE_OPUS_MODEL = %q, want %q", got, "personal:claude-opus-5.5")
	}
}

// --provider equal to the session's own subscribed provider stores the bare spec, unqualified.
func TestApplyModelPinsBareWhenProviderMatchesSession(t *testing.T) {
	hosttest.PinScratch(t)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	vaulttest.SetRecord(t, "claude", "profile", "personal")
	if code, handled := ApplyModelPins(reg, "personal", false, []string{"opus=claude-opus-5.5"}, nil); handled || code != 0 {
		t.Fatalf("ApplyModelPins: code=%d handled=%v, want 0, false", code, handled)
	}
	if got := providers.AgentRecord("claude").Aliases["opus"]; got != "claude-opus-5.5" {
		t.Errorf("CLAUDE_OPUS_MODEL = %q, want the bare spec %q", got, "claude-opus-5.5")
	}
}

// qualifyPinSpec passes an already-qualified "provider:model" spec through unchanged, even when
// --provider names a different provider than the one in the spec.
func TestQualifyPinSpecPassesThroughAlreadyQualified(t *testing.T) {
	hosttest.PinScratch(t)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	got := providers.QualifyPinSpec(t.Context(), reg, "personal", "anthropic:claude-opus-5.5")
	if got != "anthropic:claude-opus-5.5" {
		t.Errorf("qualifyPinSpec = %q, want the already-qualified spec unchanged", got)
	}
}

// --pin all=... qualifies CLAUDE_MODEL the same way a per-alias pin does.
func TestApplyModelPinsAllSlotQualifies(t *testing.T) {
	hosttest.PinScratch(t)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	vaulttest.SetRecord(t, "claude", "profile", "anthropic")
	if code, handled := ApplyModelPins(reg, "personal", false, []string{"all=claude-opus-5.5"}, nil); handled || code != 0 {
		t.Fatalf("ApplyModelPins: code=%d handled=%v, want 0, false", code, handled)
	}
	if got := providers.AgentRecord("claude").Model; got != "personal:claude-opus-5.5" {
		t.Errorf("CLAUDE_MODEL = %q, want %q", got, "personal:claude-opus-5.5")
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
