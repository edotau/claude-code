package router

import (
	"bytes"
	"maps"
	"os"
	"path/filepath"
	"reflect"
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

func TestRotationPickAndPeek(t *testing.T) {
	pool := []string{"fake-model-a", "fake-model-b"}
	for _, tc := range []struct {
		name, held, first, second string
	}{
		{"least recent", "", pool[1], pool[0]},
		{"skip held model", pool[1], pool[0], pool[0]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			p, agent := poolProvider(), "opencode"
			account := ProviderAccount(p.Name)
			now := time.Now().UTC()
			a, b := DiscoverySkipKey(agent, pool[0]), DiscoverySkipKey(agent, pool[1])
			if err := update(func(s State) {
				s[account] = Entry{Discovery: map[string]time.Time{a: now.Add(-time.Hour), b: now.Add(-2 * time.Hour)}}
			}); err != nil {
				t.Fatal(err)
			}
			oldPool := RotationPool
			RotationPool = func(string, string, *providers.Provider, bool, time.Time) []string { return pool }
			t.Cleanup(func() { RotationPool = oldPool })
			if tc.held != "" && SkipDiscovery(agent, p, tc.held) != tc.held {
				t.Fatal("SkipDiscovery did not hold the model")
			}
			checkRotationPeek(t, p, pool, now, tc.first)
			checkRotationPicks(t, p, pool, now, []string{tc.first, tc.second})
		})
	}
}

func checkRotationPeek(t *testing.T, p *providers.Provider, pool []string, now time.Time, want string) {
	t.Helper()
	before := LoadState()
	raw, err := os.ReadFile(StatePath())
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(StatePath())
	if err != nil {
		t.Fatal(err)
	}
	if got := PeekRotation("opencode", ProviderAccount(p.Name), ProviderAccount(p.Name), p, pool, now); got != want {
		t.Errorf("PeekRotation = %q, want %q", got, want)
	}
	after, err := os.ReadFile(StatePath())
	if err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(StatePath())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, after) || !info.ModTime().Equal(afterInfo.ModTime()) || !reflect.DeepEqual(before, LoadState()) {
		t.Fatal("PeekRotation changed state")
	}
}

func checkRotationPicks(t *testing.T, p *providers.Provider, pool []string, now time.Time, wants []string) {
	t.Helper()
	const agent = "opencode"
	account, before := ProviderAccount(p.Name), LoadState()
	for i, want := range wants {
		at := now.Add(time.Duration(i+1) * time.Second)
		got, err := PickRotation(agent, account, account, p, pool, at)
		if err != nil || got != want {
			t.Fatalf("PickRotation = %q, %v; want %q", got, err, want)
		}
		e := before[account]
		e.Discovery = maps.Clone(e.Discovery)
		e.Discovery[DiscoverySkipKey(agent, want)] = at
		before[account] = e
		if got := LoadState(); !reflect.DeepEqual(got, before) {
			t.Fatalf("state after pick = %+v, want %+v", got, before)
		}
	}
}

func TestPickPoolRotatesHonorsSkipAndExpiry(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	p, pool := poolProvider(), []string{"databricks-kimi-k3", "databricks-grok-4-6", "databricks-glm-5-3"}
	oldPool := RotationPool
	RotationPool = func(string, string, *providers.Provider, bool, time.Time) []string { return pool }
	t.Cleanup(func() { RotationPool = oldPool })
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

func TestDiscoveryPoolOverlay(t *testing.T) {
	vaulttest.UseKeyring(t)
	if got, ok := PoolOverlay(); ok || len(got) != 0 {
		t.Fatalf("unset overlay: pool = %v, valid = %v", got, ok)
	}
	put := func(raw string) {
		t.Helper()
		if err := vault.Update(func(tx *vault.Tx) error { return tx.Put(vault.Overlay, vault.PoolKey, raw) }); err != nil {
			t.Fatal(err)
		}
	}
	put(`["databricks-kimi-k3","databricks-not-a-model","databricks-grok-4-6"]`)
	if got, ok := PoolOverlay(); !ok || !slices.Equal(got, []string{"databricks-kimi-k3", "databricks-grok-4-6"}) {
		t.Errorf("overlay pool = %v, want catalog ids only", got)
	}
	for _, bad := range []string{`not json`, `[]`, `["databricks-not-a-model"]`} {
		put(bad)
		if got, ok := PoolOverlay(); ok || len(got) != 0 {
			t.Errorf("overlay %q: pool = %v, valid = %v", bad, got, ok)
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
