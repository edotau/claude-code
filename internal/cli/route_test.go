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

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/proxy"
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
	vaulttest.SetRecord(t, "copilot", "profile", "work")
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

// TestRouteEndsNamesResolvedClaudeAccount: the router view resolves claude's actual backend, reported as .ends.
func TestRouteEndsNamesResolvedClaudeAccount(t *testing.T) {
	vaulttest.Use(t)
	oldAcct, oldRefresh := agent.ResolveAccount, routeRefresh
	agent.ResolveAccount = func(runner string, _ router.Cell) (string, string) {
		if runner == "claude" {
			return "databricks·prod", ""
		}
		return "", ""
	}
	routeRefresh = func(context.Context, string) bool { return false }
	t.Cleanup(func() { agent.ResolveAccount, routeRefresh = oldAcct, oldRefresh })

	out, code := captureRouteStdout(t, func() int { return gatewayRoute([]string{"--model", "sonnet", "--format", "json"}, false) })
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	var res routeJSON
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if res.Ends != "databricks·prod" {
		t.Errorf("ends = %q, want databricks·prod\n%s", res.Ends, out)
	}
}

// TestRouterStartHelpDoesNotStart: `gateway proxy -h` printed nothing and spawned the daemon (seen 2026-10-23 when
// an opencode audit dispatch asked for help). The flag must print usage and leave the router alone.
func TestRouterStartHelpDoesNotStart(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, args := range [][]string{{"proxy", "-h"}, {"proxy", "--help"}, {"-h"}} {
		code, _, stderr := captureOutput(t, func() int { return cmdGateway(args) })
		if code != 0 || !strings.Contains(stderr, "usage: claude-code gateway") {
			t.Errorf("gateway %v: want usage + exit 0, got exit %d stderr %q", args, code, stderr)
		}
	}
	if _, ok := proxy.Running(context.Background()); ok {
		t.Fatal("router was started by a help request")
	}
}

// TestRouterStrayArgumentIsAUsageError: a typo after start/stop/status fails loudly instead of being ignored.
func TestRouterStrayArgumentIsAUsageError(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	code, _, stderr := captureOutput(t, func() int { return cmdGateway([]string{"proxy", "now"}) })
	if code == 0 || !strings.Contains(stderr, `unexpected argument "now"`) {
		t.Fatalf("want a usage error naming the stray argument, got exit %d stderr %q", code, stderr)
	}
}

func TestVaultStorageHelpPrintsUsage(t *testing.T) {
	useVaultTest(t)
	for _, op := range []string{"set", "get", "delete", "import", "export"} {
		code, stdout, stderr := captureOutput(t, func() int { return cmdVault([]string{op, "-h"}) })
		if code != 0 || !strings.Contains(stderr, "usage: claude-code vault "+op) || stdout != "" {
			t.Errorf("vault %s -h: want usage + exit 0 and no op output, got exit %d", op, code)
		}
	}
}

// parityEnv installs a signed-in Databricks session profile whose menu marks gpt-6-astra Responses-capable or not;
// every agent binary is a stub that drops a marker if it is run on a prompt (status probes are fine).
func parityEnv(t *testing.T, profile string, responses bool) (marker string) {
	t.Helper()
	dir := vaulttest.Use(t)
	t.Setenv("CLAUDE_PROFILE", profile)
	marker = filepath.Join(t.TempDir(), "ran")
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, b := range []string{"codex", "copilot", "opencode", "gemini"} {
		if err := os.WriteFile(filepath.Join(dir, "bin", b), []byte("#!/bin/sh\ncase \"$1\" in exec|run|-p|--prompt) touch "+marker+";; esac\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"`+profile+`":{"preset":"databricks",`+
		`"host":"https://profile.example.com","auth":{"type":"pat"},"models":{"sonnet":"gpt-6-1-sol"}}}}}`)
	if err := providers.SetOpencodeMenu([]providers.OpencodeModel{
		{ID: "gpt-6-1-sol", Label: "GPT-6.1 Sol", Responses: true},
		{ID: "gpt-6-astra", Label: "GPT-6 Astra", Responses: responses},
	}); err != nil {
		t.Fatal(err)
	}
	reg, err := providers.Load()
	if err != nil || reg.Providers[profile] == nil {
		t.Fatalf("profile absent: %v", err)
	}
	vaulttest.Secret(t, providers.AuthSecretName(reg.Providers[profile]), "profile-auth")
	if err := agent.SetAgentRecord("codex", agent.AgentRecord{Profile: "chatgpt"}); err != nil { // own login = on
		t.Fatal(err)
	}
	old := routeRefresh
	routeRefresh = func(context.Context, string) bool { return false }
	t.Cleanup(func() { routeRefresh = old })
	return marker
}

// TestRouterPreviewMatchesDispatch: `gateway router` lists the candidates, models and skips `ask` would route.
func TestRouterPreviewMatchesDispatch(t *testing.T) {
	cases := []struct {
		name, model string
		responses   bool
		wantSkip    string // a skip naming this profile must appear
	}{
		{"exact_supported", "gpt-6-astra", true, ""},
		{"exact_unsupported", "gpt-6-astra", false, "astra-par"},
		{"alias_control", "sonnet", true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			marker := parityEnv(t, "astra-par", tc.responses)
			out, code := captureRouteStdout(t, func() int {
				return gatewayRoute([]string{"--agent", "codex", "--model", tc.model, "--fallback", "agents", "--format", "json"}, false)
			})
			if code != 0 {
				t.Fatalf("preview exit %d: %s", code, out)
			}
			var got routeJSON
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("not JSON: %v\n%s", err, out)
			}
			chain, _, err := agent.RouteChainFor(context.Background(), "codex", tc.model, "sonnet", router.FallbackAgents, false,
				router.PickOpts{Refresh: routeRefresh, WorkRepo: func() bool { return router.IsWorkRepo("") }})
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Candidates) != len(chain.Dispatches) {
				t.Fatalf("preview has %d candidates, dispatch %d\n%s", len(got.Candidates), len(chain.Dispatches), out)
			}
			for i, disp := range chain.Dispatches {
				c := got.Candidates[i]
				if c.Agent != disp.Agent.Name() || c.AccountKey != disp.Account || (c.Model != disp.Model && c.Agent != "claude") {
					t.Errorf("#%d preview %s/%s/%s, dispatch %s/%s/%s", i+1, c.Agent, c.AccountKey, c.Model,
						disp.Agent.Name(), disp.Account, disp.Model)
				}
			}
			var skips []string
			for _, s := range got.Skipped {
				skips = append(skips, s.Agent+": "+s.Reason)
			}
			if strings.Join(skips, "\n") != strings.Join(chain.Skipped, "\n") {
				t.Errorf("preview skips %q, dispatch skips %q", skips, chain.Skipped)
			}
			if tc.wantSkip != "" && !strings.Contains(strings.Join(skips, "\n"), tc.wantSkip) {
				t.Errorf("no skip names profile %s: %q", tc.wantSkip, skips)
			}
			if _, err := os.Stat(marker); err == nil {
				t.Error("preview ran an agent on a prompt")
			}
		})
	}
}
