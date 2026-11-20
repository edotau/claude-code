package preflight

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/catalog"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// The vendor drivers register their roster rows from their own init, and importing them here would cycle through hooks.
func init() {
	agent.RegisterRowForTest(agent.Schema{Name: "claude"})
	agent.RegisterRowForTest(agent.Schema{Name: "codex", CLI: agent.CLI{Binary: "codex"}})
	agent.RegisterRowForTest(agent.Schema{Name: "opencode", CLI: agent.CLI{Binary: "opencode", OwnLoginProfile: "zen-free"}})
	agent.RegisterRowForTest(agent.Schema{Name: "copilot", CLI: agent.CLI{Binary: "copilot"}})
	agent.RegisterRowForTest(agent.Schema{Name: "gemini", CLI: agent.CLI{Binary: "gemini", OwnLoginProfile: "antigravity"}})
	agent.RegisterRowForTest(agent.Schema{Name: "pi", CLI: agent.CLI{Binary: "pi"}})
}

// hermeticVault gives the test its own empty vault and no session profile, so no pin from another test leaks in.
func hermeticVault(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_PROFILE", "")
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
}

// stubListing makes every profile list a ready and a cold endpoint, without the network.
func stubListing(t *testing.T) {
	t.Helper()
	refresh = func(_ context.Context, p *providers.Provider) error {
		return router.WarmServed(p.Name, []providers.ModelInfo{{ID: "good-model", Task: "llm/v1/chat", Ready: "READY"},
			{ID: "cold-model", Ready: "NOT_READY"}}, time.Now())
	}
	t.Cleanup(func() { refresh = catalog.Refresh })
}

func pinAll(t *testing.T) {
	t.Helper()
	setPins(t, map[string]string{"CLAUDE_PROFILE": "dev", "CODEX_PROFILE": "dev", "COPILOT_PROFILE": "dev", "OPENCODE_PROFILE": "dev"})
}

// An agent with no profile is off: Check skips it and the session line stays quiet.
func TestCheckSkipsAgentsWithoutProfile(t *testing.T) {
	hermeticVault(t)
	reg, now := preflightRegistry(true), time.Now()
	if got := Check(context.Background(), reg, now); len(got) != 0 {
		t.Fatalf("with no profile every external agent is off, got %+v", got)
	}
	if line := StaticLine(reg, Check(context.Background(), reg, now)); line != "" {
		t.Fatalf("static line %q, want none while no Databricks profile is in play", line)
	}
}

// Two run snapshots pick their own session profile whatever the process environment says.
func TestCheckReadsTheRunSnapshot(t *testing.T) {
	hermeticVault(t)
	t.Setenv(providers.EnvSessionProfile, "ambient")
	reg, now := preflightRegistry(true), time.Now()
	for profile, want := range map[string]string{"dev": "dev", "": ""} {
		ctx := proc.WithValues(context.Background(), proc.Values{providers.EnvSessionProfile: profile})
		got := byAgent(Check(ctx, reg, now))["claude"]
		if got.Profile != want {
			t.Errorf("snapshot profile %q: claude pin %q, want %q", profile, got.Profile, want)
		}
	}
}

// An agent on its own sign-in (codex on chatgpt, opencode on zen-free) is pinned: ok, named by its record profile.
func TestCheckOwnLoginIsPinned(t *testing.T) {
	hermeticVault(t)
	setPins(t, map[string]string{"CODEX_PROFILE": "chatgpt", "OPENCODE_PROFILE": "zen-free", "GEMINI_PROFILE": "antigravity"})
	got := byAgent(Check(context.Background(), preflightRegistry(true), time.Now()))
	if len(got) != 3 {
		t.Fatalf("want codex, opencode and gemini checked, copilot off: %+v", got)
	}
	for name, profile := range map[string]string{"codex": "chatgpt", "opencode": "zen-free", "gemini": "antigravity"} {
		if p := got[name]; p.Status != StatusOK || p.ProfileSource != "own login" || p.Profile != profile {
			t.Errorf("%s on its own login = %+v, want ok on %s", name, p, profile)
		}
	}
}

// One pin walks stale (no listing) -> ok (refreshed) -> unserved (cold endpoint) -> stale (past the TTL).
func TestCheckPinFollowsTheListing(t *testing.T) {
	hermeticVault(t)
	stubListing(t)
	reg, now := preflightRegistry(true), time.Now()
	pinAll(t)
	setPins(t, map[string]string{"CODEX_MODEL": "good-model"})

	ps := Check(context.Background(), reg, now)
	if c := byAgent(ps)["codex"]; c.ProfileSource != "pin" || c.Status != statusStale {
		t.Fatalf("codex before any listing = %+v, want stale via its profile pin", c)
	}
	if errs := RefreshStale(context.Background(), reg, ps, now, false); len(errs) != 0 {
		t.Fatal(errs)
	}
	if c := byAgent(Check(context.Background(), reg, now))["codex"]; c.Status != StatusOK || c.Served != "yes" {
		t.Fatalf("codex after refresh = %+v, want ok/served", c)
	}
	setPins(t, map[string]string{"CODEX_MODEL": "cold-model"})
	if c := byAgent(Check(context.Background(), reg, now))["codex"]; c.Status != statusUnserved || c.Detail != "endpoint not READY" {
		t.Fatalf("codex on an unready endpoint = %+v", c)
	}
	setPins(t, map[string]string{"CODEX_MODEL": "good-model"})
	if c := byAgent(Check(context.Background(), reg, now.Add(25*time.Hour)))["codex"]; c.Status != statusStale || c.Fix != "claude-code doctor --refresh" {
		t.Fatalf("codex past the TTL = %+v, want stale", c)
	}
}

// A model the workspace does not list is unserved with the discovery fix; a missing profile is stale and shows in the static line.
func TestCheckUnservedAndMissingProfile(t *testing.T) {
	hermeticVault(t)
	stubListing(t)
	reg, now := preflightRegistry(true), time.Now()
	pinAll(t)
	setPins(t, map[string]string{"COPILOT_MODEL": "gone-model"})
	if errs := RefreshStale(context.Background(), reg, Check(context.Background(), reg, now), now, false); len(errs) != 0 {
		t.Fatal(errs)
	}
	got := byAgent(Check(context.Background(), reg, now))
	if c := got["copilot"]; c.Status != statusUnserved || c.Served != "no" || !strings.Contains(c.Fix, "models --live --profile dev") {
		t.Fatalf("copilot with a missing model = %+v, want unserved with the discovery fix", c)
	}
	if got["opencode"].Status != StatusOK {
		t.Fatalf("opencode = %+v, want ok", got["opencode"])
	}
	setPins(t, map[string]string{"OPENCODE_PROFILE": "nope"})
	if c := byAgent(Check(context.Background(), reg, now))["opencode"]; c.Status != statusStale {
		t.Fatalf("opencode on a missing profile = %+v, want stale", c)
	}
	if line := StaticLine(reg, Check(context.Background(), reg, now)); !strings.Contains(line, "opencode stale") || !strings.Contains(line, "proxy down") {
		t.Fatalf("static line %q must flag opencode and the proxy", line)
	}
}

// A profile with no credential is unserved with the reason, and the install step makes no model call for it.
func TestCheckNoCredentialIsUnserved(t *testing.T) {
	hermeticVault(t)
	reg := preflightRegistry(false)
	pinAll(t)
	setPins(t, map[string]string{"CODEX_MODEL": "m"})
	if c := byAgent(Check(context.Background(), reg, time.Now()))["codex"]; c.Status != statusUnserved || !strings.Contains(c.Detail, "no credential for dev") {
		t.Fatalf("codex without a credential = %+v", c)
	}
}

func TestUnreadableRecordsSurfaceInPreflight(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, vault.AgentRecordsKey, "{")
	reg := preflightRegistry(true)
	now := time.Now()
	ps := Check(context.Background(), reg, now)
	if len(ps) != 1 || ps[0].Status != StatusError || ps[0].Served != "no" || !strings.Contains(ps[0].Detail, "agent-records is corrupt") {
		t.Fatalf("Check = %+v; want a vault error, not unpinned agents", ps)
	}
	var out bytes.Buffer
	RunInstallStep(context.Background(), &out, reg, "must-not-run", now)
	if !strings.Contains(out.String(), "vault error: agent pins: vault: agent-records is corrupt") {
		t.Fatalf("install output = %q", out.String())
	}
	if got := StaticLine(reg, ps); !strings.Contains(got, "agent-records is corrupt") {
		t.Fatalf("StaticLine = %q", got)
	}
}

// The launch-time arm never shows up in preflight: a pinned agent reports its pin even with a bad HARNESS_DATABRICKS_ARM.
func TestCheckIgnoresDatabricksArm(t *testing.T) {
	hermeticVault(t)
	setPins(t, map[string]string{"CODEX_PROFILE": "dev"})
	t.Setenv("HARNESS_DATABRICKS_ARM", "nosuch")
	c := byAgent(Check(context.Background(), preflightRegistry(true), time.Now()))["codex"]
	if c.ProfileSource != "pin" || c.Profile != "dev" || strings.Contains(c.Detail, "HARNESS_DATABRICKS_ARM") {
		t.Fatalf("codex with the arm set = %+v, want the plain pin", c)
	}
}

// Gemini on a Databricks row is checked against the listing by its serving name, without the effort suffix.
func TestCheckGeminiOnDatabricksChecksServedName(t *testing.T) {
	hermeticVault(t)
	reg, now := preflightRegistry(true), time.Now()
	if err := router.WarmServed("dev", []providers.ModelInfo{{ID: "databricks-gemini-3-8-flash", Task: "llm/v1/chat", Ready: "READY"}}, now); err != nil {
		t.Fatal(err)
	}
	for model, want := range map[string]string{"gemini-3.8-flash-high": StatusOK, "gemini-3.8-pro-high": statusUnserved} {
		if p := checkAgent(reg, "gemini", router.PinSpec{Profile: "dev", Model: model}, "dev", now); p.Status != want {
			t.Errorf("gemini %s = %+v, want %s", model, p, want)
		}
	}
}

// A claude alias pin the session workspace does not serve downgrades claude's row.
func TestCheckClaudeAliasPinIsChecked(t *testing.T) {
	hermeticVault(t)
	stubListing(t)
	reg, now := preflightRegistry(true), time.Now()
	if err := router.WarmServed("dev", []providers.ModelInfo{{ID: "good-model", Task: "llm/v1/chat", Ready: "READY"}}, now); err != nil {
		t.Fatal(err)
	}
	setPins(t, map[string]string{"CLAUDE_PROFILE": "dev", "CLAUDE_ALIASES.OPUS": "good-model"})
	if c := byAgent(Check(context.Background(), reg, now))["claude"]; c.Status != StatusOK {
		t.Fatalf("claude on a served alias pin = %+v, want ok", c)
	}
	setPins(t, map[string]string{"CLAUDE_ALIASES.SONNET": "gone-model"})
	if c := byAgent(Check(context.Background(), reg, now))["claude"]; c.Status != statusUnserved || c.Detail != "sonnet pin: not in the workspace listing" {
		t.Fatalf("claude on an unserved alias pin = %+v, want unserved naming sonnet", c)
	}
}

// A Responses-only alias pin downgrades claude's row but keeps the session profile; the pin's profile is warmed.
func TestCheckClaudeAliasPinResponsesOnly(t *testing.T) {
	hermeticVault(t)
	stubListing(t)
	reg, now := preflightRegistry(true), time.Now()
	reg.Providers["dev"].Routes = map[string]string{providers.ProtocolResponses: "https://dev.example/responses"}
	if err := router.WarmServed("dev", []providers.ModelInfo{{ID: "databricks-gpt-6-1-sol", Task: "llm/v1/chat", Ready: "READY"}}, now); err != nil {
		t.Fatal(err)
	}
	setPins(t, map[string]string{"CLAUDE_PROFILE": "dev", "CLAUDE_ALIASES.OPUS": "dev:gpt-6-1-sol"})
	c := byAgent(Check(context.Background(), reg, now))["claude"]
	if c.Status != statusUnserved || c.Profile != "dev" || !strings.Contains(c.Detail, "opus pin: model databricks-gpt-6-1-sol is Responses-only") {
		t.Fatalf("claude on a Responses-only alias pin = %+v, want unserved on profile dev naming opus", c)
	}
	if len(c.Also) != 1 || c.Also[0] != "dev" {
		t.Fatalf("Also = %v, want the alias pin's profile", c.Also)
	}
}

func TestPinRotationDetailIsLazy(t *testing.T) {
	hermeticVault(t)
	row := loginRow("codex", route.Target{Model: agent.AutoModel}, time.Now())
	if row.Detail != "" || row.rotationDetail == nil {
		t.Fatalf("auto row = detail %q, rotationDetail nil %v; want the summary deferred", row.Detail, row.rotationDetail == nil)
	}
	calls := 0
	row.rotationDetail = func() string { calls++; return "rot-detail" }
	if calls != 0 {
		t.Fatalf("detail called %d times before Line, want 0", calls)
	}
	if line := row.Line(); !strings.Contains(line, "· rot-detail") || calls != 1 {
		t.Fatalf("Line = %q after %d calls, want the detail once", line, calls)
	}
}
