package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	copilot "github.com/github/copilot-sdk/go"

	"github.com/edotau/claude-code/internal/copilotsdk"
	"github.com/edotau/claude-code/internal/providers"
)

func registryRouteStubs(t *testing.T) (minted *int) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("COPILOT_HOME", "")
	oldD, oldE, oldM := copilotDiscover, copilotEnsureRouter, copilotMintToken
	t.Cleanup(func() { copilotDiscover, copilotEnsureRouter, copilotMintToken = oldD, oldE, oldM })
	copilotDiscover = func(context.Context, *providers.Registry, *providers.Provider) ([]CopilotEndpoint, error) {
		var ep CopilotEndpoint
		ep.ID, ep.Name = "databricks-claude-sonnet-4", "s"
		return []CopilotEndpoint{ep}, nil
	}
	copilotEnsureRouter = func(context.Context) (string, error) { return "http://127.0.0.1:1", nil }
	minted = new(int)
	copilotMintToken = func(context.Context, string, int) (string, error) { *minted++; return "tok", nil }
	return minted
}

// An unregistered model fails the SDK registry route before any token is minted, and a registered one yields
// Options whose child env drops an inherited COPILOT_PROVIDERS_CONFIG / GitHub token / singular BYOK block.
func TestCopilotRegistryRouteFailsClosedAndScrubs(t *testing.T) {
	t.Cleanup(WithCopilotOS("linux")) // the macOS refusal has its own test
	minted := registryRouteStubs(t)
	up := &providers.Provider{Name: "dbx"}

	_, err := prepareCopilotRegistry(context.Background(), providers.Target{Provider: up, Model: "databricks-no-such-model"})
	if !errors.Is(err, ErrUnknownModel) || *minted != 0 {
		t.Fatalf("err = %v, minted = %d; want ErrUnknownModel and no mint", err, *minted)
	}

	rt, err := prepareCopilotRegistry(context.Background(), providers.Target{Provider: up, Model: "databricks-claude-sonnet-4"})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("COPILOT_PROVIDERS_CONFIG", "/tmp/stale.json")
	t.Setenv("COPILOT_PROVIDER_BASE_URL", "http://stale")
	t.Setenv("GITHUB_TOKEN", "gh-secret")
	t.Setenv("GH_TOKEN", "gh-secret")
	o := copilotsdk.Options{CLIPath: "/fake/copilot", GitHubToken: "gh-secret", UseLoggedInUser: true, Env: []string{"GH_TOKEN=x"}}
	rt.apply(&o)
	if o.GitHubToken != "" || o.UseLoggedInUser {
		t.Fatalf("registry route kept a GitHub login: %+v", o)
	}
	co, err := copilotsdk.BuildClientOptions(o)
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range co.Connection.(copilot.StdioConnection).Env {
		if strings.HasPrefix(kv, "COPILOT_PROVIDERS_CONFIG=") || strings.HasPrefix(kv, "COPILOT_PROVIDER_") ||
			strings.HasPrefix(kv, "GITHUB_TOKEN=") || strings.HasPrefix(kv, "GH_TOKEN=") || strings.Contains(kv, "gh-secret") {
			t.Errorf("child env kept %q", kv)
		}
	}
}
