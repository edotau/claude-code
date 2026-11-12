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
