package cli

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

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
