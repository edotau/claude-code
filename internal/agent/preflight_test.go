package agent

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

	"github.com/edotau/claude-code/internal/providers"
)

func preflightRegistry(cred bool) *providers.Registry {
	p := &providers.Provider{Name: "dev", Preset: providers.PresetDatabricks}
	if cred {
		p.Auth = providers.Auth{Type: "key", Command: "echo x"}
	}
	return &providers.Registry{Providers: map[string]*providers.Provider{"dev": p}}
}

func setPins(t *testing.T, kv map[string]string) {
	t.Helper()
	if err := providers.SetPins(kv); err != nil {
		t.Fatal(err)
	}
}

func byAgent(ps []PreflightPin) map[string]PreflightPin {
	out := map[string]PreflightPin{}
	for _, p := range ps {
		out[p.Agent] = p
	}
	return out
}

// One spanning flow: unpinned -> pinned without a listing -> refreshed listing -> unserved model -> expired TTL.
func TestPinsFlow(t *testing.T) {
	t.Setenv("CLAUDE_PROFILE", "")
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // own vault: pins from other tests must not leak in
	now := time.Now()
	reg := preflightRegistry(true)
	listModels = func(context.Context, *providers.Registry, *providers.Provider) ([]providers.ModelInfo, error) {
		return []providers.ModelInfo{{ID: "good-model", Task: "llm/v1/chat", Ready: "READY"}, {ID: "cold-model", Ready: "NOT_READY"}}, nil
	}
	t.Cleanup(func() { listModels = providers.ListModels })

	got := byAgent(PreflightCheck(reg, now))
	for _, a := range PreflightAgents {
		if got[a].Status != PreflightStatusUnpinned || !strings.Contains(got[a].Fix, "claude-code "+a+" profile") {
			t.Fatalf("%s with no pins = %+v, want unpinned with a profile fix", a, got[a])
		}
	}
	if line := StaticLine(reg, now); line != "" {
		t.Fatalf("static line %q, want none while no Databricks profile is in play", line)
	}

	setPins(t, map[string]string{
		"CLAUDE_PROFILE": "dev", "COPILOT_PROFILE": "dev", "OPENCODE_PROFILE": "dev",
		"CODEX_MODEL": "good-model", "COPILOT_MODEL": "gone-model",
	})
	ps := PreflightCheck(reg, now)
	got = byAgent(ps)
	if got["codex"].ProfileSource != "follows CLAUDE_PROFILE" || got["codex"].Status != PreflightStatusStale {
		t.Fatalf("codex before any listing = %+v, want stale via CLAUDE_PROFILE", got["codex"])
	}
	if errs := RefreshStale(context.Background(), reg, ps, now, false); len(errs) != 0 {
		t.Fatal(errs)
	}
	got = byAgent(PreflightCheck(reg, now))
	if got["codex"].Status != PreflightStatusOK || got["codex"].Served != "yes" {
		t.Fatalf("codex after refresh = %+v, want ok/served", got["codex"])
	}
	if c := got["copilot"]; c.Status != PreflightStatusUnserved || c.Served != "no" || !strings.Contains(c.Fix, "models --live --profile dev") {
		t.Fatalf("copilot with a missing model = %+v, want unserved with the discovery fix", c)
	}
	if got["opencode"].Status != PreflightStatusOK {
		t.Fatalf("opencode = %+v, want ok", got["opencode"])
	}

	setPins(t, map[string]string{"CODEX_MODEL": "cold-model"})
	if c := byAgent(PreflightCheck(reg, now))["codex"]; c.Status != PreflightStatusUnserved || c.Detail != "endpoint not READY" {
		t.Fatalf("codex on an unready endpoint = %+v", c)
	}
	setPins(t, map[string]string{"CODEX_MODEL": "good-model"})
	if c := byAgent(PreflightCheck(reg, now.Add(ServedTTL+time.Hour)))["codex"]; c.Status != PreflightStatusStale || c.Fix != "claude-code doctor --refresh" {
		t.Fatalf("codex past the TTL = %+v, want stale", c)
	}
	setPins(t, map[string]string{"OPENCODE_PROFILE": "nope"})
	if c := byAgent(PreflightCheck(reg, now))["opencode"]; c.Status != PreflightStatusStale {
		t.Fatalf("opencode on a missing profile = %+v, want stale", c)
	}
	if line := StaticLine(reg, now); !strings.Contains(line, "opencode stale") || !strings.Contains(line, "proxy not running") {
		t.Fatalf("static line %q must flag opencode and the proxy", line)
	}
	setPins(t, map[string]string{"OPENCODE_PROFILE": "dev"})

	var asked []string
	runAsk = func(_ context.Context, _, a string) (envelope, error) {
		asked = append(asked, a)
		if a == "opencode" {
			return envelope{Status: "error", Error: &struct {
				Kind string `json:"kind"`
			}{"auth"}}, nil
		}
		fb := "gemini"
		return envelope{Status: "ok", Provider: "dev", Model: "good-model", Fallback: &fb}, nil
	}
	var buf bytes.Buffer
	RunInstallStep(context.Background(), &buf, reg, "claude-code", now)
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

// With no credential the install step makes no model call and the pin says why.
func TestNoCredentialSkips(t *testing.T) {
	t.Setenv("CLAUDE_PROFILE", "")
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // own vault: pins from other tests must not leak in
	reg := preflightRegistry(false)
	setPins(t, map[string]string{"CLAUDE_PROFILE": "dev", "COPILOT_PROFILE": "dev", "OPENCODE_PROFILE": "dev", "CODEX_MODEL": "m"})
	runAsk = func(context.Context, string, string) (envelope, error) {
		t.Error("ask must not run without a credential")
		return envelope{}, nil
	}
	var buf bytes.Buffer
	RunInstallStep(context.Background(), &buf, reg, "claude-code", time.Now())
	if buf.Len() != 0 {
		t.Fatalf("want a silent skip, got %q", buf.String())
	}
	if c := byAgent(PreflightCheck(reg, time.Now()))["codex"]; c.Status != PreflightStatusUnserved || !strings.Contains(c.Detail, "no credential") {
		t.Fatalf("codex without a credential = %+v", c)
	}
}

// A pin must match the listing the way the runtime spells it: no blind databricks- stripping.
func TestHasDoesNotStripPrefix(t *testing.T) {
	s := Served{Models: []ServedModel{{ID: "databricks-my-endpoint", Ready: "READY"}}}
	if listed, _ := s.Has(preflightRegistry(true).Providers["dev"], "my-endpoint"); listed {
		t.Fatal("pin my-endpoint must not match databricks-my-endpoint")
	}
}

// A listing stamped in the future is stale, not fresh forever.
func TestFutureListingIsStale(t *testing.T) {
	now := time.Now()
	if (Served{FetchedAt: now.Add(48 * time.Hour)}).Fresh(now) {
		t.Fatal("future fetched_at must not be fresh")
	}
	if !(Served{FetchedAt: now.Add(time.Minute)}).Fresh(now) {
		t.Fatal("small skew must stay fresh")
	}
}

// Refresh and pongs share one budget; a hung listing cannot push the step past it.
func TestInstallStepSharesBudget(t *testing.T) {
	t.Setenv("CLAUDE_PROFILE", "")
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // own vault: pins from other tests must not leak in
	setPins(t, map[string]string{"CLAUDE_PROFILE": "dev", "COPILOT_PROFILE": "dev", "OPENCODE_PROFILE": "dev", "CODEX_MODEL": "m"})
	old := PreflightBudget
	PreflightBudget = 300 * time.Millisecond
	t.Cleanup(func() { PreflightBudget = old; listModels = providers.ListModels })
	listModels = func(ctx context.Context, _ *providers.Registry, _ *providers.Provider) ([]providers.ModelInfo, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	runAsk = func(ctx context.Context, _, _ string) (envelope, error) { <-ctx.Done(); return envelope{}, ctx.Err() }
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
