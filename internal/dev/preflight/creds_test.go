package preflight

import (
	"context"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/routing/providers"
)

// startPath is the SessionStart pin work: Check, the stale probe and the static line over one registry.
func startPath(reg *providers.Registry, now time.Time) {
	ps := Check(context.Background(), reg, now)
	Stale(reg, ps, now)
	StaticLine(reg, ps)
}

func TestCredentialMemoResetSameRegistry(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	const env = "PREFLIGHT_MEMO_TEST_KEY"
	t.Setenv(env, "present")
	p := &providers.Provider{Auth: providers.Auth{Type: "key", Env: env}}
	reg := &providers.Registry{}
	resetCreds(reg)
	t.Cleanup(func() { resetCreds(nil) })
	if !credentialPresent(reg, p) {
		t.Fatal("credential should be present")
	}
	t.Setenv(env, "")
	if !credentialPresent(reg, p) {
		t.Fatal("same run should reuse its probe")
	}
	resetCreds(reg)
	if credentialPresent(reg, p) {
		t.Fatal("new run must not reuse the previous run's credential")
	}
}

func BenchmarkStartPath(b *testing.B) {
	b.Setenv("CLAUDE_CONFIG_DIR", b.TempDir())
	reg, now := preflightRegistry(true), time.Now()
	reg.Providers["dev"].Auth = providers.Auth{Type: "key", Env: "PREFLIGHT_BENCH_UNSET"}
	b.Setenv("CLAUDE_PROFILE", "dev")
	b.ReportAllocs()
	for b.Loop() {
		startPath(reg, now)
	}
}
