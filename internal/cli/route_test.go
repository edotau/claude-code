package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// captureStdout runs fn with os.Stdout redirected and returns what it printed.
func captureRouteStdout(t *testing.T, fn func() int) (string, int) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	code := fn()
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	return string(out), code
}

// TestRouteVerbJSON: the pick's JSON carries weights, per-dimension scores, totals and each skip's gate.
func TestRouteVerbJSON(t *testing.T) {
	dir := vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"github":{"work":{"protocol":"openai",
		"base_url":"https://api.githubcopilot.com","auth":{"type":"cli","login":"copilot"},"login":"work-user"}}}}`)
	vaulttest.Put(t, vault.Pins, providers.PinCopilotProfile, "work")
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, b := range []string{"copilot", "codex"} {
		if err := os.WriteFile(filepath.Join(dir, "bin", b), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	routeRefresh = func(context.Context, string) bool { return false }
	ghHosts(t, "work-user")
	t.Cleanup(func() { routeRefresh = nil })
	if err := router.MarkExhausted("github·work", time.Now().Add(240*time.Hour), time.Now(), "402"); err != nil {
		t.Fatal(err)
	}
	out, code := captureRouteStdout(t, func() int {
		return cmdGatewayRouter([]string{"--model", "sonnet", "--agent", "copilot", "--format", "json"})
	})
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	var res routeJSON
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if res.Model != "sonnet" || res.Weights["price"] == 0 || len(res.Candidates) == 0 {
		t.Fatalf("res = %+v", res)
	}
	var skip *skipJSON
	for i := range res.Skipped {
		if res.Skipped[i].Agent == "copilot" {
			skip = &res.Skipped[i]
		}
	}
	if skip == nil || skip.Gate != router.GateExhausted || !strings.Contains(skip.Reason, "github·work exhausted until") {
		t.Errorf("copilot skip = %+v in %+v", skip, res.Skipped)
	}
	if c := res.Candidates[len(res.Candidates)-1]; c.Agent != "claude" || c.Model != "claude-sonnet-5-5" || c.Scores == nil {
		t.Errorf("last candidate = %+v, want claude at sonnet with scores", c)
	}
	if code := cmdGatewayRouter([]string{"--model", "fable"}); code == 0 {
		t.Error("an unrouted tier must fail")
	}
}

// TestRouteVerbSourcesNameWhereEachKnobCameFrom checks `route`'s header names weights' and subagents'
// provenance: default when the vault overlay never set them, vault/env once it does.
func TestRouteVerbSourcesNameWhereEachKnobCameFrom(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(providers.EnvRouteSubagents, "")

	out, code := captureRouteStdout(t, func() int { return cmdGatewayRouter([]string{"--model", "sonnet", "--format", "json"}) })
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	var withDefaults struct {
		Sources struct {
			Weights   string `json:"weights"`
			Subagents struct {
				On     bool
				Source string
			} `json:"subagents"`
		} `json:"sources"`
	}
	if err := json.Unmarshal([]byte(out), &withDefaults); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if withDefaults.Sources.Weights != "default" || withDefaults.Sources.Subagents.Source != "default" || !withDefaults.Sources.Subagents.On {
		t.Errorf("sources (no overlay) = %+v, want weights/subagents default, subagents on", withDefaults.Sources)
	}

	if err := providers.UpdateRouting(func(r *providers.Routing) error {
		r.Weights = map[string]float64{"price": 0.5, "headroom": 0.5} // equal to the default: provenance is still "vault"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv(providers.EnvRouteSubagents, "0")

	out, code = captureRouteStdout(t, func() int { return cmdGatewayRouter([]string{"--model", "sonnet", "--format", "json"}) })
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	var withOverlay struct {
		Sources struct {
			Weights   string `json:"weights"`
			Subagents struct {
				On     bool
				Source string
			} `json:"subagents"`
		} `json:"sources"`
	}
	if err := json.Unmarshal([]byte(out), &withOverlay); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if withOverlay.Sources.Weights != "vault" {
		t.Errorf("weights source = %q, want vault even though the value equals the default", withOverlay.Sources.Weights)
	}
	if withOverlay.Sources.Subagents.Source != "env" || withOverlay.Sources.Subagents.On {
		t.Errorf("subagents = %+v, want off/env (the process env beats the vault)", withOverlay.Sources.Subagents)
	}

	textOut, code := captureRouteStdout(t, func() int { return cmdGatewayRouter([]string{"--model", "sonnet"}) })
	if code != 0 {
		t.Fatalf("exit %d: %s", code, textOut)
	}
	if !strings.Contains(textOut, "(vault)") || !strings.Contains(textOut, "subagents off (env)") {
		t.Errorf("text output = %q, want it to name the vault weights source and env subagents source", textOut)
	}
}

// ghHosts makes user the only gh login: a scratch hosts.yml under GH_CONFIG_DIR.
func ghHosts(t *testing.T, user string) {
	t.Helper()
	dir := t.TempDir()
	hosts := "github.com:\n    user: " + user + "\n    users:\n        " + user + ":\n            oauth_token: tok\n"
	if err := os.WriteFile(filepath.Join(dir, "hosts.yml"), []byte(hosts), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GH_CONFIG_DIR", dir)
}
