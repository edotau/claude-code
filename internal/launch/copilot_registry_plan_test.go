package launch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/agent/vendors"
)

// registryFixture serves a Databricks workspace over TLS (the one network boundary) with one READY chat endpoint,
// and installs a fake copilot binary so a wrongful passthrough to the plain CLI would succeed.
func registryFixture(t *testing.T) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"endpoints": []map[string]any{
			{"name": "databricks-claude-sonnet-5-5", "task": "llm/v1/chat", "state": map[string]string{"ready": "READY"}},
		}})
	}))
	t.Cleanup(srv.Close)
	old := http.DefaultTransport
	http.DefaultTransport = srv.Client().Transport // trusts the test server's certificate
	t.Cleanup(func() { http.DefaultTransport = old })

	row := `"dbx":{"preset":"databricks","host":"` + srv.URL + `","auth":{"type":"key","env":"TEST_GW_KEY"},` +
		`"models":{"opus":"databricks-claude-sonnet-5-5","sonnet":"databricks-claude-sonnet-5-5"}}`
	hermetic(t, `{`+noModels+`,"providers":{"databricks":{`+row+`}}}`)
	t.Setenv("TEST_GW_KEY", "dbx-key")
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	pathstest.FakeVendor(t, "copilot", "exit 0")
	vendors.StubRouterForTest(t, "http://127.0.0.1:4000") // the mint still reaches the real, spawn-refusing Ensure
}

// The registry route fails closed: a mint failure and an unregistered --model each error out, and neither
// falls through to the plain copilot CLI (which would run on its own GitHub login).
func TestCopilotRegistryPlanFailsClosed(t *testing.T) {
	t.Cleanup(vendors.WithCopilotOS("linux")) // the macOS refusal has its own test
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"unregistered model", []string{"--model", "no-such-model"}, "no-such-model"},
		{"mint failure", nil, "session token for profile dbx"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registryFixture(t)
			plan, err := Vendor(context.Background(), "copilot", Options{Provider: "dbx"}, tc.args)
			if err == nil {
				t.Fatalf("plan = %+v, want an error, not a launch", plan)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to name %q", err, tc.want)
			}
			if plan.Binary != "" || len(plan.Argv) != 0 {
				t.Errorf("failed registry launch still carries a plan: %+v", plan)
			}
		})
	}
}

// Whatever the route, an inherited COPILOT_PROVIDERS_CONFIG and the GitHub token vars never reach the child:
// the own-login (plain) route scrubs them, and the registry route's Unset list names every one.
func TestCopilotChildEnvScrubbed(t *testing.T) {
	t.Cleanup(vendors.WithCopilotOS("linux")) // the macOS refusal has its own test
	hermetic(t, "")
	inherited := []string{"COPILOT_PROVIDERS_CONFIG=/tmp/stale.json", "COPILOT_PROVIDER_BASE_URL=http://x",
		"GH_TOKEN=gh-secret", "GITHUB_TOKEN=gh-secret", "COPILOT_GITHUB_TOKEN=gh-secret", "KEEP_ME=1"}
	has := func(env []string, name string) bool {
		for _, kv := range env {
			if strings.HasPrefix(kv, name+"=") {
				return true
			}
		}
		return false
	}

	t.Run("own-login route", func(t *testing.T) {
		pathstest.FakeVendor(t, "copilot", "exit 0")
		plan, err := Vendor(context.Background(), "copilot", Options{}, []string{"login"})
		if err != nil {
			t.Fatal(err)
		}
		env := plan.Env(inherited)
		for _, n := range []string{"COPILOT_PROVIDERS_CONFIG", "COPILOT_PROVIDER_BASE_URL", "COPILOT_GITHUB_TOKEN"} { // GH_TOKEN/GITHUB_TOKEN are the own login's credentials and pass
			if has(env, n) {
				t.Errorf("%s survived the own-login scrub: %v", n, env)
			}
		}
		if !has(env, "KEEP_ME") {
			t.Errorf("scrub dropped an unrelated var: %v", env)
		}
	})

	t.Run("registry route", func(t *testing.T) {
		ph, err := vendors.PrepareCopilotHome(true)
		if err != nil {
			t.Fatal(err)
		}
		// Same shape copilotRegistryPlan builds: the registry file is Set, the private-home Unset list rides along.
		plan := Plan{Vendor: true, Set: map[string]string{"COPILOT_PROVIDERS_CONFIG": "/run/own.json"}, Unset: ph.Unset}
		env := plan.Env(inherited)
		if !has(env, "COPILOT_PROVIDERS_CONFIG") {
			t.Fatalf("registry file missing: %v", env)
		}
		for _, kv := range env {
			if strings.Contains(kv, "stale.json") || strings.Contains(kv, "gh-secret") || strings.HasPrefix(kv, "COPILOT_PROVIDER_BASE_URL=") {
				t.Errorf("inherited %q reached the registry child", kv)
			}
		}
	})
}

// A failed exec must run OnExecFail (the registry launch revokes its session token there).
func TestPlanExecFailureRunsOnExecFail(t *testing.T) {
	hermetic(t, "")
	called := false
	err := Plan{Binary: "/nonexistent/copilot", Argv: []string{"copilot"}, OnExecFail: func() { called = true }}.Exec()
	if err == nil {
		t.Fatal("exec of a missing binary succeeded")
	}
	if !called {
		t.Error("OnExecFail did not run after the exec failed")
	}
}
