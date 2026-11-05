package router

import (
	"os"
	"path/filepath"
	"slices"
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
	if err := vault.Update(func(tx *vault.Tx) error { return tx.Put(vault.Overlay, "byok-models", `["m-a","m-b","m-c"]`) }); err != nil {
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
	if m := SkipBYOK("opencode", p, "m-a"); m != "m-a" {
		t.Fatalf("SkipBYOK = %q, want m-a", m)
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
	if m, _ := pickPool("opencode", p, only, now.Add(byokSkipTTL+time.Hour)); m != "m-a" {
		t.Errorf("after the 7-day TTL pick = %q, want m-a back in rotation", m)
	}
}

func TestBYOKPoolOverlayOverridesSeedAndSeedIsCloned(t *testing.T) {
	vaulttest.UseKeyring(t)
	got := BYOKPool()
	if !slices.Equal(got, DatabricksBYOKPool) {
		t.Fatalf("unset overlay: pool = %v, want the seed %v", got, DatabricksBYOKPool)
	}
	got[0] = "mutated"
	if DatabricksBYOKPool[0] == "mutated" {
		t.Fatal("BYOKPool aliases the seed slice")
	}
	put := func(raw string) {
		t.Helper()
		if err := vault.Update(func(tx *vault.Tx) error { return tx.Put(vault.Overlay, byokPoolKey, raw) }); err != nil {
			t.Fatal(err)
		}
	}
	put(`["databricks-a","databricks-b"]`)
	if got := BYOKPool(); !slices.Equal(got, []string{"databricks-a", "databricks-b"}) {
		t.Errorf("overlay pool = %v", got)
	}
	for _, bad := range []string{`not json`, `[]`} {
		put(bad)
		if got := BYOKPool(); !slices.Equal(got, DatabricksBYOKPool) {
			t.Errorf("overlay %q: pool = %v, want the seed", bad, got)
		}
	}
}

func TestSkipBYOKReportsNothingWhenSkipNotPersisted(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	vaulttest.UseKeyring(t)
	if err := vault.Update(func(tx *vault.Tx) error { return tx.Put(vault.Overlay, byokPoolKey, `["m-a"]`) }); err != nil {
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
	if m := SkipBYOK("opencode", poolProvider(), "m-a"); m != "" {
		t.Fatalf("SkipBYOK with an unwritable quota.json = %q, want empty", m)
	}
}
