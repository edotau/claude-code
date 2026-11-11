package opencode

import (
	"slices"
	"testing"

	"github.com/edotau/claude-code/internal/hooks"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// TestProvisionalSessionIsUniquePerDispatch pins that two dispatches in the same second never share hook state.
func TestProvisionalSessionIsUniquePerDispatch(t *testing.T) {
	a, b := provisionalSession(), provisionalSession()
	if a == b {
		t.Errorf("back-to-back provisional ids are equal (%q): concurrent dispatches would share hook state", a)
	}
}

// hostedBound keeps only registry-hosted modules whose group is on; safe mode keeps the gates alone.
func TestHostedBoundFiltersByRegistrySwitchAndSafeMode(t *testing.T) {
	vaulttest.Use(t)
	var hosted string
	for name := range hostedHooks {
		hosted = name
	}
	if hosted == "" {
		t.Fatal("the registry hosts no opencode hook module")
	}
	orig := bindHooks
	t.Cleanup(func() { bindHooks = orig })
	bindHooks = func() []hooks.Bound {
		return []hooks.Bound{
			{Group: "on", Spec: settings.EventSpec{Name: hosted}},
			{Group: "off", Spec: settings.EventSpec{Name: hosted}},
			{Group: "off-gate", Gate: true, Spec: settings.EventSpec{Name: hosted}},
			{Group: "foreign-gate", Gate: true, Spec: settings.EventSpec{Name: "not-hosted"}},
		}
	}
	for safe, want := range map[bool][]string{false: {"on", "off", "off-gate"}, true: {"off-gate"}} {
		var got []string
		for _, b := range hostedBound(safe) {
			got = append(got, b.Group)
		}
		if !slices.Equal(got, want) {
			t.Errorf("safeMode=%v: bound groups %v, want %v", safe, got, want)
		}
	}
}
