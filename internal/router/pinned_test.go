package router

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func poolProvider() *providers.Provider {
	return &providers.Provider{Name: "dbx", Provider: providers.GroupDatabricks, Preset: providers.PresetDatabricks,
		Auth: providers.Auth{Type: providers.AuthKey, Env: "X"}}
}

func TestPickPoolRotatesHonorsSkipAndExpiry(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	p, pool := poolProvider(), []string{"databricks-kimi-k3", "databricks-grok-4-6", "databricks-glm-5-3"}
	if err := vault.Update(func(tx *vault.Tx) error {
		return tx.Put(vault.Overlay, vault.PoolKey, `["databricks-kimi-k3","databricks-grok-4-6","databricks-glm-5-3"]`)
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	var got []string
	for i := range 3 {
		m, err := pickPool("opencode", p, pool, now.Add(time.Duration(i)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, m)
	}
	if !slices.Equal(got, pool) {
		t.Fatalf("3 picks = %v, want pool order %v (ties by pool order, then least recent)", got, pool)
	}
	if m := SkipDiscovery("opencode", p, "databricks-kimi-k3"); m != "databricks-kimi-k3" {
		t.Fatalf("SkipDiscovery = %q, want kimi-k3", m)
	}
	if m, _ := pickPool("copilot", p, pool, now.Add(9*time.Second)); m != "databricks-kimi-k3" {
		t.Errorf("skip is per agent: copilot pick = %q, want kimi-k3", m)
	}
	if m, _ := pickPool("opencode", p, pool, now.Add(10*time.Second)); m != "databricks-grok-4-6" {
		t.Errorf("pick with kimi-k3 skipped = %q, want grok-4-6", m)
	}
	only := []string{"databricks-kimi-k3"}
	if m, _ := pickPool("opencode", p, only, now.Add(time.Hour)); m != "" {
		t.Errorf("live skip: pick = %q, want none", m)
	}
	if m, _ := pickPool("opencode", p, only, now.Add(DiscoverySkipTTL+time.Hour)); m != "databricks-kimi-k3" {
		t.Errorf("after the 7-day TTL pick = %q, want kimi-k3 back in rotation", m)
	}
}

func TestDiscoveryPoolOverlayOverridesSeedAndSeedIsCloned(t *testing.T) {
	vaulttest.UseKeyring(t)
	got := DiscoveryPool()
	if !slices.Equal(got, databricksDiscoveryPool) {
		t.Fatalf("unset overlay: pool = %v, want the seed %v", got, databricksDiscoveryPool)
	}
	got[0] = "mutated"
	if databricksDiscoveryPool[0] == "mutated" {
		t.Fatal("DiscoveryPool aliases the seed slice")
	}
	put := func(raw string) {
		t.Helper()
		if err := vault.Update(func(tx *vault.Tx) error { return tx.Put(vault.Overlay, vault.PoolKey, raw) }); err != nil {
			t.Fatal(err)
		}
	}
	put(`["databricks-kimi-k3","databricks-not-a-model","databricks-grok-4-6"]`)
	if got := DiscoveryPool(); !slices.Equal(got, []string{"databricks-kimi-k3", "databricks-grok-4-6"}) {
		t.Errorf("overlay pool = %v, want catalog ids only", got)
	}
	for _, bad := range []string{`not json`, `[]`, `["databricks-not-a-model"]`} {
		put(bad)
		if got := DiscoveryPool(); !slices.Equal(got, databricksDiscoveryPool) {
			t.Errorf("overlay %q: pool = %v, want the seed", bad, got)
		}
	}
}

func TestSkipDiscoveryReportsNothingWhenSkipNotPersisted(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	vaulttest.UseKeyring(t)
	if err := vault.Update(func(tx *vault.Tx) error { return tx.Put(vault.Overlay, vault.PoolKey, `["databricks-kimi-k3"]`) }); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(StatePath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(StatePath()+".lock", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(StatePath()), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Dir(StatePath()), 0o755) })
	if os.WriteFile(filepath.Join(filepath.Dir(StatePath()), "probe"), nil, 0o600) == nil {
		t.Skip("state dir stays writable (running as root)")
	}
	if m := SkipDiscovery("opencode", poolProvider(), "databricks-kimi-k3"); m != "" {
		t.Fatalf("SkipDiscovery with an unwritable quota.json = %q, want empty", m)
	}
}

func TestSkipDiscoveryReportsPersistenceFailure(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	// a directory where quota.json belongs: reads fail
	if err := os.MkdirAll(StatePath(), 0o700); err != nil {
		t.Fatal(err)
	}
	var got string
	_, _, out := hosttest.CaptureOutput(t, func() int { got = SkipDiscovery("opencode", poolProvider(), "databricks-kimi-k3"); return 0 })
	if got != "" || !strings.Contains(string(out), "could not persist model skip") {
		t.Errorf("got %q, stderr %q; want empty result and a persistence message", got, out)
	}
}
