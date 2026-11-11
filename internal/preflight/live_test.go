package preflight

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/wire"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func preflightRegistry(cred bool) *providers.Registry {
	p := &providers.Provider{Name: "dev", Preset: providers.PresetDatabricks}
	if cred {
		p.Auth = providers.Auth{Type: "key", Command: "echo x"}
	}
	ag := &providers.Provider{Name: "antigravity", Provider: "google", Auth: providers.Auth{Type: providers.AuthCLI, Login: "gemini"}}
	return &providers.Registry{Providers: map[string]*providers.Provider{"dev": p, "antigravity": ag}}
}

// setPins sets agent record fields from "<AGENT>_<FIELD>" keys (CODEX_MODEL sets codex's model).
func setPins(t *testing.T, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		agent, field, _ := strings.Cut(strings.ToLower(k), "_")
		vaulttest.SetRecord(t, agent, field, v)
	}
}

func byAgent(ps []Pin) map[string]Pin {
	out := map[string]Pin{}
	for _, p := range ps {
		out[p.Agent] = p
	}
	return out
}

// A pin must match the listing the way the runtime spells it: no blind databricks- stripping.
func TestHasDoesNotStripPrefix(t *testing.T) {
	s := router.Served{Models: []router.ServedModel{{ID: "databricks-my-endpoint", Ready: "READY"}}}
	if listed, _ := s.Has(preflightRegistry(true).Providers["dev"], "my-endpoint"); listed {
		t.Fatal("pin my-endpoint must not match databricks-my-endpoint")
	}
}

// A listing stamped in the future is stale, not fresh forever.
func TestFutureListingIsStale(t *testing.T) {
	now := time.Now()
	if (router.Served{FetchedAt: now.Add(48 * time.Hour)}).Fresh(now) {
		t.Fatal("future fetched_at must not be fresh")
	}
	if !(router.Served{FetchedAt: now.Add(time.Minute)}).Fresh(now) {
		t.Fatal("small skew must stay fresh")
	}
}

// Killing a timed-out ask must also kill its children, or the pipe keeps Output() blocked.
func TestRunAskKillsProcessGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups are unix-only")
	}
	script := filepath.Join(t.TempDir(), "fake")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 30 &\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _ = defaultRunAsk(ctx, script, "codex", "")
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("runAsk blocked %v on an orphaned child", d)
	}
}

// The install step prints one pong row per live agent (80 columns at most) and a fix row for a pin that is not ok.
func TestInstallStepRows(t *testing.T) {
	hermeticVault(t)
	stubListing(t)
	pinAll(t)
	setPins(t, map[string]string{"CODEX_MODEL": "good-model", "COPILOT_MODEL": "gone-model"})
	var asked []string
	runAsk = func(_ context.Context, _, a, _ string) (wire.Envelope, error) {
		asked = append(asked, a)
		if a == "opencode" {
			return wire.Envelope{Status: "error", Error: &wire.EnvelopeError{Kind: "auth"}}, nil
		}
		fb := "gemini"
		return wire.Envelope{Status: "ok", Provider: "dev", Model: "good-model", Fallback: &fb}, nil
	}
	var buf bytes.Buffer
	RunInstallStep(context.Background(), &buf, preflightRegistry(true), "claude-code", time.Now())
	out := buf.String()
	if !strings.Contains(out, "pong codex    · dev · good-model · ok · gemini") ||
		!strings.Contains(out, "pong opencode · dev · default · fail · auth") ||
		!strings.Contains(out, "copilot unserved: ") || slices.Contains(asked, "copilot") {
		t.Fatalf("install step rows:\n%s", out)
	}
	for l := range strings.SplitSeq(out, "\n") {
		if strings.HasPrefix(l, "pong ") && len([]rune(l)) > 80 {
			t.Fatalf("row over 80 columns: %q", l)
		}
	}
}

// With no credential anywhere the install step is silent and never asks a model.
func TestInstallStepSkipsWithoutCredential(t *testing.T) {
	hermeticVault(t)
	pinAll(t)
	runAsk = func(context.Context, string, string, string) (wire.Envelope, error) {
		t.Error("ask must not run without a credential")
		return wire.Envelope{}, nil
	}
	reg := preflightRegistry(false)
	delete(reg.Providers, "antigravity") // a CLI-login pin counts as a credential
	var buf bytes.Buffer
	RunInstallStep(context.Background(), &buf, reg, "claude-code", time.Now())
	if buf.Len() != 0 {
		t.Fatalf("want a silent skip, got %q", buf.String())
	}
}

// Refresh and pongs share one budget; a hung listing cannot push the step past it.
func TestInstallStepSharesBudget(t *testing.T) {
	hermeticVault(t)
	pinAll(t)
	setPins(t, map[string]string{"CODEX_MODEL": "m"})
	old := Budget
	Budget = 300 * time.Millisecond
	t.Cleanup(func() { Budget = old; listModels = providers.ListModels })
	listModels = func(ctx context.Context, _ *providers.Provider) ([]providers.ModelInfo, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	runAsk = func(ctx context.Context, _, _, _ string) (wire.Envelope, error) {
		<-ctx.Done()
		return wire.Envelope{}, ctx.Err()
	}
	var buf bytes.Buffer
	start := time.Now()
	RunInstallStep(context.Background(), &buf, preflightRegistry(true), "claude-code", time.Now().Add(48*time.Hour))
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("step took %v, want within the shared budget", d)
	}
	if !strings.Contains(buf.String(), "fail · timeout") {
		t.Fatalf("want timeout rows, got %q", buf.String())
	}
}

// opencode's install pong may outlast Budget (cold start); the other agents still stop at Budget.
func TestInstallStepOpencodePongOutlastsBudget(t *testing.T) {
	hermeticVault(t)
	stubListing(t)
	pinAll(t)
	old := Budget
	Budget = 200 * time.Millisecond
	t.Cleanup(func() { Budget = old })
	runAsk = func(ctx context.Context, _, a, _ string) (wire.Envelope, error) {
		select {
		case <-time.After(3 * Budget / 2): // past Budget, inside opencode's 2x cap
			return wire.Envelope{Status: "ok", Provider: "dev", Model: "m"}, nil
		case <-ctx.Done():
			return wire.Envelope{}, ctx.Err()
		}
	}
	var buf bytes.Buffer
	RunInstallStep(context.Background(), &buf, preflightRegistry(true), "claude-code", time.Now())
	out := buf.String()
	if !strings.Contains(out, "pong opencode · dev · m · ok") || !strings.Contains(out, "pong codex    · dev · default · fail · timeout") {
		t.Fatalf("want opencode ok and codex timeout:\n%s", out)
	}
}

// --profile all pongs claude, codex, copilot, gemini and opencode on each usable Databricks profile, passing --profile.
func TestRunProfilePongsAllExpandsProfiles(t *testing.T) {
	reg := preflightRegistry(true)
	for _, n := range []string{"qa", "prod"} {
		reg.Providers[n] = &providers.Provider{Name: n, Provider: "databricks", Preset: providers.PresetDatabricks, Auth: reg.Providers["dev"].Auth}
	}
	reg.Providers["dev"].Provider = "databricks"
	var mu sync.Mutex
	asked := map[string]bool{}
	runAsk = func(_ context.Context, _, a, prof string) (wire.Envelope, error) {
		mu.Lock()
		asked[a+"/"+prof] = true
		mu.Unlock()
		return wire.Envelope{Status: "ok", Provider: prof, Model: "m"}, nil
	}
	var buf bytes.Buffer
	RunProfilePongs(context.Background(), &buf, reg, "claude-code", "all")
	if len(asked) != 15 || !asked["gemini/qa"] || !asked["claude/prod"] || strings.Count(buf.String(), " · ok · ") != 15 {
		t.Fatalf("asked %v\n%s", asked, buf.String())
	}
}

// A pin on a CLI-login profile (gemini's Google login) counts as a credential, so the bare install step still pongs.
func TestInstallStepPongsCLILoginPin(t *testing.T) {
	hermeticVault(t)
	var asked []string
	runAsk = func(_ context.Context, _, a, _ string) (wire.Envelope, error) {
		asked = append(asked, a)
		return wire.Envelope{Status: "ok", Provider: "antigravity", Model: "m"}, nil
	}
	var buf bytes.Buffer
	RunInstallStep(context.Background(), &buf, preflightRegistry(false), "claude-code", time.Now())
	if !slices.Equal(asked, []string{"gemini"}) || !strings.Contains(buf.String(), "pong gemini") {
		t.Fatalf("asked %v\n%s", asked, buf.String())
	}
}

// --profile all prints a skip row for a Databricks profile without a credential, and for a named unknown profile.
func TestRunProfilePongsSkipRows(t *testing.T) {
	reg := preflightRegistry(true)
	reg.Providers["dev"].Provider = "databricks"
	reg.Providers["prod"] = &providers.Provider{Name: "prod", Provider: "databricks", Preset: providers.PresetDatabricks}
	runAsk = func(_ context.Context, _, _, prof string) (wire.Envelope, error) {
		return wire.Envelope{Status: "ok", Provider: prof, Model: "m"}, nil
	}
	var buf bytes.Buffer
	RunProfilePongs(context.Background(), &buf, reg, "claude-code", "all")
	RunProfilePongs(context.Background(), &buf, reg, "claude-code", "typo")
	out := buf.String()
	if !strings.Contains(out, "pong skip · prod · no credential") || !strings.Contains(out, "pong skip · typo · unknown profile") {
		t.Fatalf("missing skip rows:\n%s", out)
	}
}

// A profile run where no pong can run ends in a fail row (doctor exits non-zero on it); a usable profile adds none.
func TestRunProfilePongsFailRowWhenNonePong(t *testing.T) {
	reg := preflightRegistry(true)
	reg.Providers["dev"].Provider = "databricks"
	runAsk = func(_ context.Context, _, _, prof string) (wire.Envelope, error) {
		return wire.Envelope{Status: "ok", Provider: prof, Model: "m"}, nil
	}
	var bad, good bytes.Buffer
	RunProfilePongs(context.Background(), &bad, reg, "claude-code", "typo")
	RunProfilePongs(context.Background(), &good, reg, "claude-code", "dev")
	if !strings.Contains(bad.String(), "pong · fail · typo · no usable Databricks profile") {
		t.Fatalf("want a fail row for an unusable profile, got %q", bad.String())
	}
	if strings.Contains(good.String(), " · fail · ") {
		t.Fatalf("usable profile must not emit a fail row:\n%s", good.String())
	}
}

// TestMain runs in the hermetic host (hosttest): no live provider pins, vault or vendor CLIs are reachable.
func TestMain(m *testing.M) { os.Exit(hosttest.Main(m)) }
