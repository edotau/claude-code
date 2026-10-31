package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

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
			chain, _, err := agent.RouteChainFor(context.Background(), "codex", tc.model, "sonnet", agent.FallbackAgents, false,
				agent.PickOpts{Refresh: routeRefresh, WorkRepo: func() bool { return agent.IsWorkRepo("") }})
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Candidates) != len(chain.Legs) {
				t.Fatalf("preview has %d candidates, dispatch %d\n%s", len(got.Candidates), len(chain.Legs), out)
			}
			for i, leg := range chain.Legs {
				c := got.Candidates[i]
				if c.Agent != leg.Agent.Name() || c.AccountKey != leg.Account || (c.Model != leg.Model && c.Agent != "claude") {
					t.Errorf("#%d preview %s/%s/%s, dispatch %s/%s/%s", i+1, c.Agent, c.AccountKey, c.Model,
						leg.Agent.Name(), leg.Account, leg.Model)
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
