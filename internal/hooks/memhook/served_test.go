package memhook

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/preflight"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// servedSession seeds a hermetic vault and a dev Databricks session; it returns the served-refresh spawn count.
func servedSession(t *testing.T, on bool, fetched time.Time, updateChild string) *atomic.Int64 {
	t.Helper()
	vaulttest.Use(t)
	t.Setenv(memory.UpdateChildEnv, updateChild)
	row := `{"routing":{"servedAware":%t},"providers":{"databricks":{"dev":{"preset":"databricks",` +
		`"host":"https://dev.cloud.databricks.com","auth":{"type":"pat"}}}}}`
	vaulttest.Put(t, vault.Overlay, "providers", fmt.Sprintf(row, on))
	vaulttest.Secret(t, "DATABRICKS_DEV_PAT", "dapi-test")
	t.Setenv(providers.EnvSessionProfile, "dev")
	if !fetched.IsZero() {
		if err := router.SaveServed(router.Served{Profile: "dev", FetchedAt: fetched}); err != nil {
			t.Fatal(err)
		}
	}
	calls := new(atomic.Int64)
	orig := spawnServed
	spawnServed = func(string) bool { calls.Add(1); return true }
	t.Cleanup(func() { spawnServed = orig })
	return calls
}

// A start re-lists only with the switch on, a stale pinned Databricks cache, and outside an update child.
func TestSessionStartRelistsServedOnlyForStaleSessionProfile(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name        string
		on          bool
		fetched     time.Time
		updateChild string
		want        int64
	}{
		{"stale cache, switch on", true, time.Time{}, "", 1},
		{"expired cache, switch on", true, now.Add(-48 * time.Hour), "", 1},
		{"fresh cache", true, now.Add(-time.Minute), "", 0},
		{"switch off", false, time.Time{}, "", 0},
		{"update child", true, time.Time{}, "1", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			calls := servedSession(t, c.on, c.fetched, c.updateChild)
			reg, _ := providers.Load()
			refreshServed(reg, preflight.Check(context.Background(), reg, now), t.TempDir(), now)
			if got := calls.Load(); got != c.want {
				t.Fatalf("spawnServed calls = %d, want %d", got, c.want)
			}
		})
	}
}

// A failing refresh leaves the cache stale; the next SessionStart inside the retry window must not spawn again.
func TestSessionStartClaimsServedRefreshOncePerRetryWindow(t *testing.T) {
	now := time.Now()
	calls := servedSession(t, true, time.Time{}, "")
	reg, _ := providers.Load()
	refreshServed(reg, preflight.Check(context.Background(), reg, now), t.TempDir(), now)
	refreshServed(reg, preflight.Check(context.Background(), reg, now), t.TempDir(), now.Add(time.Minute))
	if got := calls.Load(); got != 1 {
		t.Fatalf("spawnServed calls = %d, want 1", got)
	}
}

// TestSessionStartLoadsProvidersOnce: the served refresh and the pin check share one registry load per start.
func TestSessionStartLoadsProvidersOnce(t *testing.T) {
	calls := servedSession(t, true, time.Time{}, "")
	loads := 0
	orig := loadProviders
	loadProviders = func() (*providers.Registry, error) { loads++; return orig() }
	t.Cleanup(func() { loadProviders = orig })
	Start(startInput(t.TempDir(), "startup"))
	if loads != 1 || calls.Load() != 1 {
		t.Fatalf("registry loads = %d, served refreshes = %d; want 1 and 1", loads, calls.Load())
	}
}
