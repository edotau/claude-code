package agent

import (
	"context"
	"fmt"
	"testing"

	"github.com/edotau/claude-code/internal/agent/retry"

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
	if gerr == nil || gerr.Kind != retry.KindFatal {
		t.Fatalf("isolated guardAttempt = %v, want KindFatal", gerr)
	}
	if _, gerr := guardAttempt(context.Background(), r, RunOptions{}); gerr != nil {
		t.Fatalf("non-isolated guardAttempt = %v, want nil", gerr)
	}
}

// hmProfile overlays a Databricks profile "hm" on host adb-1 whose credential comes from auth.command cmd.
func hmProfile(t *testing.T, cmd string) { hmNamed(t, "hm", cmd) }

// hmNamed is hmProfile under name; command credentials are cached per name for the process, so reruns need fresh ones.
func hmNamed(t *testing.T, name, cmd string) {
	t.Helper()
	vaulttest.UseKeyring(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"`+name+`":{"preset":"databricks",`+
		`"host":"https://adb-1.azuredatabricks.net","auth":{"type":"key","command":`+fmt.Sprintf("%q", cmd)+`},`+
		`"models":{"opus":"databricks-claude-opus-5-5","sonnet":"databricks-claude-sonnet-5-5","haiku":"databricks-claude-haiku-4-5"}}}}}`)
}

// A routed provider:model request on a vendor agent is guarded as that provider, not skipped as an own login.
func TestGuardProviderResolvesModelPrefixForVendorAgent(t *testing.T) {
	hmProfile(t, "true")
	p, err := guardProvider(&fakeRunner{name: "codex"}, RunOptions{Model: "hm:opus"})
	if err != nil || p == nil || p.Name != "hm" {
		t.Fatalf("guardProvider = %v, %v; want the hm profile", p, err)
	}
}
