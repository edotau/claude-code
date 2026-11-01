package agent

import (
	"context"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// A preset provider whose credential command fails must fail the host guard, not skip it.
func TestGuardRouteHostFailsOnCredentialError(t *testing.T) {
	vaulttest.UseKeyring(t)
	p := &providers.Provider{Name: "dbx", Preset: providers.PresetDatabricks, Auth: providers.Auth{Type: "key", Command: "false"}}
	if err := guardRouteHost(context.Background(), p); err == nil {
		t.Fatal("guardRouteHost = nil, want the credential error")
	}
}

// An isolated hop fails up front when the registry cannot load, since the foreign-credential scrub would be skipped.
func TestCheckIsolationFailsWhenRegistryUnreadable(t *testing.T) {
	vaulttest.UseKeyring(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{not json`)
	if err := checkIsolation(withIsolation(context.Background())); err == nil {
		t.Fatal("checkIsolation = nil, want the registry load error")
	}
	if err := checkIsolation(context.Background()); err != nil {
		t.Fatalf("non-isolated ctx: err = %v, want nil", err)
	}
}

// An isolated own-login hop (no provider) must also fail up front on an unreadable registry, not run unscrubbed.
func TestGuardAttemptOwnLoginIsolatedFailsWhenRegistryUnreadable(t *testing.T) {
	vaulttest.UseKeyring(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{not json`)
	r := &fakeRunner{}
	_, gerr := guardAttempt(withIsolation(context.Background()), r, RunOptions{})
	if gerr == nil || gerr.Kind != KindFatal {
		t.Fatalf("isolated guardAttempt = %v, want KindFatal", gerr)
	}
	if _, gerr := guardAttempt(context.Background(), r, RunOptions{}); gerr != nil {
		t.Fatalf("non-isolated guardAttempt = %v, want nil", gerr)
	}
}
