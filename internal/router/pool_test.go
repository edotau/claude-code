package router

import (
	"slices"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
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
