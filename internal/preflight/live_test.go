package preflight

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func preflightRegistry(cred bool) *providers.Registry {
	p := &providers.Provider{Name: "dev", Preset: providers.PresetDatabricks}
	if cred {
		p.Auth = providers.Auth{Type: "key", Command: "echo x"}
	}
	return &providers.Registry{Providers: map[string]*providers.Provider{"dev": p}}
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
	_, _ = defaultRunAsk(ctx, script, "codex")
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
	runAsk = func(_ context.Context, _, a string) (agent.Envelope, error) {
		asked = append(asked, a)
		if a == "opencode" {
			return agent.Envelope{Status: "error", Error: &agent.EnvelopeError{Kind: "auth"}}, nil
		}
		fb := "gemini"
		return agent.Envelope{Status: "ok", Provider: "dev", Model: "good-model", Fallback: &fb}, nil
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
	runAsk = func(context.Context, string, string) (agent.Envelope, error) {
		t.Error("ask must not run without a credential")
		return agent.Envelope{}, nil
	}
	var buf bytes.Buffer
	RunInstallStep(context.Background(), &buf, preflightRegistry(false), "claude-code", time.Now())
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
	listModels = func(ctx context.Context, _ *providers.Registry, _ *providers.Provider) ([]providers.ModelInfo, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	runAsk = func(ctx context.Context, _, _ string) (agent.Envelope, error) {
		<-ctx.Done()
		return agent.Envelope{}, ctx.Err()
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
