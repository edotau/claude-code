package router

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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
	p, pool := poolProvider(), []string{"m-a", "m-b", "m-c"}
	if err := vault.Update(func(tx *vault.Tx) error { return tx.Put(vault.Overlay, vault.PoolKey, `["m-a","m-b","m-c"]`) }); err != nil {
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
	if m := SkipDiscovery("opencode", p, "m-a"); m != "m-a" {
		t.Fatalf("SkipDiscovery = %q, want m-a", m)
	}
	if m, _ := pickPool("copilot", p, pool, now.Add(9*time.Second)); m != "m-a" {
		t.Errorf("skip is per agent: copilot pick = %q, want m-a", m)
	}
	if m, _ := pickPool("opencode", p, pool, now.Add(10*time.Second)); m != "m-b" {
		t.Errorf("pick with m-a skipped = %q, want m-b", m)
	}
	only := []string{"m-a"}
	if m, _ := pickPool("opencode", p, only, now.Add(time.Hour)); m != "" {
		t.Errorf("live skip: pick = %q, want none", m)
	}
	if m, _ := pickPool("opencode", p, only, now.Add(discoverySkipTTL+time.Hour)); m != "m-a" {
		t.Errorf("after the 7-day TTL pick = %q, want m-a back in rotation", m)
	}
}

func TestDiscoveryPoolOverlayOverridesSeedAndSeedIsCloned(t *testing.T) {
	vaulttest.UseKeyring(t)
	got := DiscoveryPool()
	if !slices.Equal(got, DatabricksDiscoveryPool) {
		t.Fatalf("unset overlay: pool = %v, want the seed %v", got, DatabricksDiscoveryPool)
	}
	got[0] = "mutated"
	if DatabricksDiscoveryPool[0] == "mutated" {
		t.Fatal("DiscoveryPool aliases the seed slice")
	}
	put := func(raw string) {
		t.Helper()
		if err := vault.Update(func(tx *vault.Tx) error { return tx.Put(vault.Overlay, vault.PoolKey, raw) }); err != nil {
			t.Fatal(err)
		}
	}
	put(`["databricks-a","databricks-b"]`)
	if got := DiscoveryPool(); !slices.Equal(got, []string{"databricks-a", "databricks-b"}) {
		t.Errorf("overlay pool = %v", got)
	}
	for _, bad := range []string{`not json`, `[]`} {
		put(bad)
		if got := DiscoveryPool(); !slices.Equal(got, DatabricksDiscoveryPool) {
			t.Errorf("overlay %q: pool = %v, want the seed", bad, got)
		}
	}
}

func TestSkipDiscoveryReportsNothingWhenSkipNotPersisted(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	vaulttest.UseKeyring(t)
	if err := vault.Update(func(tx *vault.Tx) error { return tx.Put(vault.Overlay, vault.PoolKey, `["m-a"]`) }); err != nil {
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
	if m := SkipDiscovery("opencode", poolProvider(), "m-a"); m != "" {
		t.Fatalf("SkipDiscovery with an unwritable quota.json = %q, want empty", m)
	}
}

func TestSkipDiscoveryReportsPersistenceFailure(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := os.MkdirAll(StatePath(), 0o700); err != nil { // a directory where quota.json belongs: reads fail
		t.Fatal(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	got := SkipDiscovery("opencode", poolProvider(), "m-a")
	os.Stderr = orig
	w.Close()
	out, _ := io.ReadAll(r)
	if got != "" || !strings.Contains(string(out), "could not persist model skip") {
		t.Errorf("got %q, stderr %q; want empty result and a persistence message", got, out)
	}
}
