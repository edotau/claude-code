package memory

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/edotau/claude-code/internal/claude" // registers the claude row
	"github.com/edotau/claude-code/internal/memory/bank"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// servedSession seeds a hermetic vault and a CLAUDE_PROFILE=dev Databricks session (switch on or off), optionally with
// a listing fetched at fetched, and returns the count of detached refresh spawns a SessionStart makes.
func servedSession(t *testing.T, on bool, fetched time.Time, updateChild string) *atomic.Int64 {
	t.Helper()
	vaulttest.Use(t)
	t.Setenv(bank.UpdateChildEnv, updateChild)
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

// SessionStart re-lists detached only when the switch is on, a pinned Databricks profile's cache is stale, and the
// session is not a memory update child.
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
			memoryContext(t.TempDir(), "s1", "", now)
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
	memoryContext(t.TempDir(), "s1", "", now)
	memoryContext(t.TempDir(), "s2", "", now.Add(time.Minute))
	if got := calls.Load(); got != 1 {
		t.Fatalf("spawnServed calls = %d, want 1", got)
	}
}
