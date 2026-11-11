package preflight

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// The vendor drivers register their roster rows from their own init, and importing them here would cycle through hooks.
func init() {
	agent.RegisterRowForTest(agent.Schema{Name: "claude"})
	agent.RegisterRowForTest(agent.Schema{Name: "codex", CLI: agent.CLI{Binary: "codex"}})
	agent.RegisterRowForTest(agent.Schema{Name: "opencode", CLI: agent.CLI{Binary: "opencode", OwnLoginProfile: "zen-free"}})
	agent.RegisterRowForTest(agent.Schema{Name: "copilot", CLI: agent.CLI{Binary: "copilot"}})
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
	listModels = func(context.Context, *providers.Provider) ([]providers.ModelInfo, error) {
		return []providers.ModelInfo{{ID: "good-model", Task: "llm/v1/chat", Ready: "READY"}, {ID: "cold-model", Ready: "NOT_READY"}}, nil
	}
	t.Cleanup(func() { listModels = providers.ListModels })
}

func pinAll(t *testing.T) {
	t.Helper()
	setPins(t, map[string]string{"CLAUDE_PROFILE": "dev", "CODEX_PROFILE": "dev", "COPILOT_PROFILE": "dev", "OPENCODE_PROFILE": "dev"})
}

// An agent with no profile is off: Check skips it and the session line stays quiet.
func TestCheckSkipsAgentsWithoutProfile(t *testing.T) {
	hermeticVault(t)
	reg, now := preflightRegistry(true), time.Now()
	if got := Check(reg, now); len(got) != 1 || got[0].Agent != "gemini" || got[0].Status != StatusOK {
		t.Fatalf("with no profile every external agent but gemini (Google pin) is off, got %+v", got)
	}
	if line := StaticLine(reg, now); line != "" {
		t.Fatalf("static line %q, want none while no Databricks profile is in play", line)
	}
}

// An agent on its own sign-in (codex on chatgpt, opencode on zen-free) is checked but unpinned: no Databricks pin.
func TestCheckOwnLoginIsUnpinned(t *testing.T) {
	hermeticVault(t)
	setPins(t, map[string]string{"CODEX_PROFILE": "chatgpt", "OPENCODE_PROFILE": "zen-free"})
	got := byAgent(Check(preflightRegistry(true), time.Now()))
	if len(got) != 3 {
		t.Fatalf("want codex, opencode and gemini checked, copilot off: %+v", got)
	}
	for _, name := range []string{"codex", "opencode"} {
		if p := got[name]; p.Status != StatusUnpinned || p.ProfileSource != "none" || p.Profile != "" || p.Detail != "own login, not Databricks" {
			t.Errorf("%s on its own login = %+v, want unpinned with no profile", name, p)
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

	ps := Check(reg, now)
	if c := byAgent(ps)["codex"]; c.ProfileSource != "pin" || c.Status != StatusStale {
		t.Fatalf("codex before any listing = %+v, want stale via its profile pin", c)
	}
	if errs := RefreshStale(context.Background(), reg, ps, now, false); len(errs) != 0 {
		t.Fatal(errs)
	}
	if c := byAgent(Check(reg, now))["codex"]; c.Status != StatusOK || c.Served != "yes" {
		t.Fatalf("codex after refresh = %+v, want ok/served", c)
	}
	setPins(t, map[string]string{"CODEX_MODEL": "cold-model"})
	if c := byAgent(Check(reg, now))["codex"]; c.Status != StatusUnserved || c.Detail != "endpoint not READY" {
		t.Fatalf("codex on an unready endpoint = %+v", c)
	}
	setPins(t, map[string]string{"CODEX_MODEL": "good-model"})
	if c := byAgent(Check(reg, now.Add(25*time.Hour)))["codex"]; c.Status != StatusStale || c.Fix != "claude-code doctor --refresh" {
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
	if errs := RefreshStale(context.Background(), reg, Check(reg, now), now, false); len(errs) != 0 {
		t.Fatal(errs)
	}
	got := byAgent(Check(reg, now))
	if c := got["copilot"]; c.Status != StatusUnserved || c.Served != "no" || !strings.Contains(c.Fix, "models --live --profile dev") {
		t.Fatalf("copilot with a missing model = %+v, want unserved with the discovery fix", c)
	}
	if got["opencode"].Status != StatusOK {
		t.Fatalf("opencode = %+v, want ok", got["opencode"])
	}
	setPins(t, map[string]string{"OPENCODE_PROFILE": "nope"})
	if c := byAgent(Check(reg, now))["opencode"]; c.Status != StatusStale {
		t.Fatalf("opencode on a missing profile = %+v, want stale", c)
	}
	if line := StaticLine(reg, now); !strings.Contains(line, "opencode stale") || !strings.Contains(line, "proxy not running") {
		t.Fatalf("static line %q must flag opencode and the proxy", line)
	}
}

// A profile with no credential is unserved with the reason, and the install step makes no model call for it.
func TestCheckNoCredentialIsUnserved(t *testing.T) {
	hermeticVault(t)
	reg := preflightRegistry(false)
	pinAll(t)
	setPins(t, map[string]string{"CODEX_MODEL": "m"})
	if c := byAgent(Check(reg, time.Now()))["codex"]; c.Status != StatusUnserved || !strings.Contains(c.Detail, "no credential for dev") {
		t.Fatalf("codex without a credential = %+v", c)
	}
}

func TestUnreadableRecordsSurfaceInPreflight(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, vault.AgentRecordsKey, "{")
	reg := preflightRegistry(true)
	now := time.Now()
	ps := Check(reg, now)
	if len(ps) != 1 || ps[0].Status != StatusError || ps[0].Served != "no" || !strings.Contains(ps[0].Detail, "agent-records is corrupt") {
		t.Fatalf("Check = %+v; want a vault error, not unpinned agents", ps)
	}
	var out bytes.Buffer
	RunInstallStep(context.Background(), &out, reg, "must-not-run", now)
	if !strings.Contains(out.String(), "vault error: agent pins: vault: agent-records is corrupt") {
		t.Fatalf("install output = %q", out.String())
	}
	if got := StaticLine(reg, now); !strings.Contains(got, "agent-records is corrupt") {
		t.Fatalf("StaticLine = %q", got)
	}
}

// The launch-time arm never shows up in preflight: a pinned agent reports its pin even with a bad HARNESS_DATABRICKS_ARM.
func TestCheckIgnoresDatabricksArm(t *testing.T) {
	hermeticVault(t)
	setPins(t, map[string]string{"CODEX_PROFILE": "dev"})
	t.Setenv("HARNESS_DATABRICKS_ARM", "nosuch")
	c := byAgent(Check(preflightRegistry(true), time.Now()))["codex"]
	if c.ProfileSource != "pin" || c.Profile != "dev" || strings.Contains(c.Detail, "HARNESS_DATABRICKS_ARM") {
		t.Fatalf("codex with the arm set = %+v, want the plain pin", c)
	}
}
