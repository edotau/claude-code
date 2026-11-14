package configure

import (
	"strings"
	"testing"

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
	got := providers.QualifyPinSpec(reg, "personal", "anthropic:claude-opus-5.5")
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

// An edit whose record has an invalid launch env is refused before the vault is written; a valid one is stored.
func TestSetAgentPinRefusesInvalidLaunchEnvBeforeStoring(t *testing.T) {
	hosttest.PinScratch(t)
	bad := vault.AgentRecord{Launch: &vault.AgentLaunch{Env: []string{"NO_EQUALS_SIGN"}}}
	stored := func() string {
		var raw string
		if err := vault.View(func(tx *vault.Tx) error { raw, _ = tx.Get(vault.Overlay, vault.AgentRecordsKey); return nil }); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	for _, c := range []struct{ field, val string }{{"MODEL", "pinned"}, {"PROFILE", "anthropic"}} {
		t.Run(c.field, func(t *testing.T) {
			if err := vault.SetAgentRecord("claude", bad); err != nil {
				t.Fatal(err)
			}
			before := stored()
			changed, err := SetAgentPin("claude", c.field, c.val)
			if err == nil || !strings.Contains(err.Error(), "launch.env") || changed {
				t.Fatalf("SetAgentPin(%s) = (%v, %v), want a launch.env refusal", c.field, changed, err)
			}
			if after := stored(); after != before {
				t.Errorf("refused edit changed the stored records:\n before %s\n after  %s", before, after)
			}
		})
	}
	good := vault.AgentRecord{Launch: &vault.AgentLaunch{Env: []string{"SYNTHETIC_FLAG=1"}}}
	if err := vault.SetAgentRecord("claude", good); err != nil {
		t.Fatal(err)
	}
	if changed, err := SetAgentPin("claude", "MODEL", "pinned"); err != nil || !changed {
		t.Fatalf("SetAgentPin on a valid record = (%v, %v), want stored", changed, err)
	}
	if got := vault.GetAgentRecord("claude").Model; got != "pinned" {
		t.Errorf("model = %q, want pinned", got)
	}
}
