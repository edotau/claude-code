package launchplan_test

import (
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/launchplan"
	"github.com/edotau/claude-code/internal/agentconfig"
	_ "github.com/edotau/claude-code/internal/claude"
	_ "github.com/edotau/claude-code/internal/codex"
	"github.com/edotau/claude-code/internal/copilot"
	_ "github.com/edotau/claude-code/internal/gemini"
	"github.com/edotau/claude-code/internal/hosttest"
	_ "github.com/edotau/claude-code/internal/opencode"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

var pidPath = regexp.MustCompile(`/\d+\.json`)

// norm blanks per-process paths and secret values so only their keys are compared.
func norm(env map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range env {
		switch {
		case strings.Contains(k, "API_KEY") || strings.Contains(k, "TOKEN"):
			v = "<secret>"
		default:
			v = pidPath.ReplaceAllString(v, "/PID.json")
		}
		out[k] = v
	}
	return out
}

func setup(t *testing.T, ag string) {
	t.Helper()
	hosttest.HermeticEnv(t)
	t.Cleanup(agent.StubRouter("http://127.0.0.1:4000"))
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"endpoints":[{"name":"databricks-gpt-6-1-sol","task":"llm/v1/chat","state":{"ready":"READY"}},{"name":"databricks-claude-opus-5-5","task":"llm/v1/chat","state":{"ready":"READY"}}]}`))
	}))
	old := http.DefaultClient.Transport
	http.DefaultClient.Transport = srv.Client().Transport
	t.Cleanup(func() { http.DefaultClient.Transport = old; srv.Close() })
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"dev":{"preset":"databricks",`+
		`"host":"`+srv.URL+`","auth":{"type":"pat"}},"qa":{"preset":"databricks",`+
		`"host":"`+srv.URL+`","auth":{"type":"pat"}}}}}`)
	pathstest.FakeVendor(t, ag, "")
	vaulttest.Secret(t, "DATABRICKS_DEV_PAT", "dapi-test")
	vaulttest.Secret(t, "DATABRICKS_QA_PAT", "dapi-test")
	vaulttest.SetRecord(t, ag, "profile", "dev")
}

// TestMapMatchesLaunch pins Map to the launch plan the interactive path builds, for the pin and a profile flag.
func TestMapMatchesLaunch(t *testing.T) {
	for _, ag := range []string{"codex", "copilot", "opencode"} {
		for _, flag := range []string{"", "qa"} {
			t.Run(ag+"/"+flag, func(t *testing.T) {
				setup(t, ag)
				if ag == "copilot" {
					t.Cleanup(copilot.StubGOOS("linux")) // the registry guard refuses darwin
				}
				want, err := launchplan.LaunchPlan(context.Background(), ag, agent.LaunchOptions{PrintEnv: true, Provider: flag}, nil)
				if err != nil {
					t.Fatal(err)
				}
				cfg, err := agentconfig.For(ag, agentconfig.Launch{Profile: flag})
				if err != nil {
					t.Fatal(err)
				}
				got, err := launchplan.Map(cfg)
				if err != nil {
					t.Fatal(err)
				}
				if len(want.Set) == 0 || len(got.Env) == 0 {
					t.Fatalf("nothing compared: plan env %d, map env %d", len(want.Set), len(got.Env))
				}
				if !maps.Equal(norm(got.Env), norm(want.Set)) {
					t.Errorf("env: map %v\nplan %v", norm(got.Env), norm(want.Set))
				}
				if !slices.Equal(pidFix(got.Args), pidFix(want.Argv)) {
					t.Errorf("args: map %v\nplan %v", got.Args, want.Argv)
				}
				if !slices.Equal(got.Unset, want.Unset) {
					t.Errorf("unset: map %v plan %v", got.Unset, want.Unset)
				}
			})
		}
	}
}

func pidFix(a []string) []string {
	return slices.Collect(func(yield func(string) bool) {
		for _, s := range a {
			if !yield(pidPath.ReplaceAllString(s, "/PID.json")) {
				return
			}
		}
	})
}

// TestMapGemini compares gemini's unpinned own-login launch (its routed profile mints a session, stubbed here).
func TestMapGemini(t *testing.T) {
	hosttest.HermeticEnv(t)
	pathstest.FakeVendor(t, "gemini", "")
	home, _ := os.UserHomeDir()
	marker := filepath.Join(home, ".gemini", "antigravity-cli", "antigravity-oauth-token")
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	want, err := launchplan.LaunchPlan(context.Background(), "gemini", agent.LaunchOptions{PrintEnv: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := agentconfig.For("gemini", agentconfig.Launch{})
	got, err := launchplan.Map(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(want.Argv) == 0 || !slices.Equal(got.Args, want.Argv) || !maps.Equal(got.Env, want.Set) {
		t.Errorf("map %v %v, plan %v %v", got.Args, got.Env, want.Argv, want.Set)
	}
}

func TestMapUnknownAgent(t *testing.T) {
	if _, err := launchplan.Map(agentconfig.Effective{Agent: "nope"}); err == nil {
		t.Fatal("want error")
	}
}

// TestMapHonorsConfigModel pins that Config.Model, not the vault pin, selects codex's model.
func TestMapHonorsConfigModel(t *testing.T) {
	setup(t, "codex")
	vaulttest.SetRecord(t, "codex", "model", "gpt-pinned")
	got, err := launchplan.Map(agentconfig.Effective{Agent: "codex", Profile: "dev", Model: "gpt-flagged"})
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString(strings.Join(got.Args, " "))
	for k, v := range got.Env {
		b.WriteString(" " + k + "=" + v)
	}
	all := b.String()
	if !strings.Contains(all, "gpt-flagged") || strings.Contains(all, "gpt-pinned") {
		t.Errorf("want Config.Model gpt-flagged only, got args %v env %v", got.Args, norm(got.Env))
	}
}
