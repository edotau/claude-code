package copilot

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// pinsScratch isolates CLAUDE_CONFIG_DIR, PATH and the provider credential env so pin checks never touch the host.
func pinsScratch(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	for _, k := range []string{"OPENAI_API_KEY", "GEMINI_API_KEY", "ANTHROPIC_API_KEY", "OPENROUTER_API_KEY", providers.EnvSessionProfile} {
		t.Setenv(k, "")
	}
}

func TestValidateDatabricksCopilotPin(t *testing.T) {
	served := map[string][]string{
		"dev": {"databricks-kimi-k3", "databricks-gpt-6-1-sol"},
	}
	for _, tc := range []struct {
		model   string
		wantErr bool
		hint    string
	}{
		{model: "kimi-k3"},
		{model: "databricks-kimi-k3"},
		{model: "kimi-k4", wantErr: true},
		{model: "kimi", wantErr: true, hint: "did you mean: databricks-kimi-k3"},
	} {
		t.Run(tc.model, func(t *testing.T) {
			_, err := validateDatabricksCopilotPin(&providers.Registry{}, "dev", tc.model, served)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateDatabricksCopilotPin(%q) error = %v, wantErr %v", tc.model, err, tc.wantErr)
			}
			if tc.hint != "" && (err == nil || !strings.Contains(err.Error(), tc.hint)) {
				t.Errorf("error = %v, want it to contain %q", err, tc.hint)
			}
		})
	}
}

func TestCopilotModelIDsReportsMissingAgentBin(t *testing.T) {
	bin := pathstest.FakeVendor(t, "copilot", "exit 99")
	t.Setenv("PATH", filepath.Dir(bin))
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	ids, err := copilotModelIDs()
	if ids != nil {
		t.Errorf("ids = %v, want nil when bin/copilot is missing", ids)
	}
	want := "copilot CLI not found: copilot: not installed at " +
		filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "bin", "copilot") +
		" (claude-code install --tools=copilot)"
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
}

// A pin naming a provider other than github-copilot is never checked against the copilot catalog.
func TestValidateCopilotPinsSkipsOtherProviders(t *testing.T) {
	pinsScratch(t)
	warn, err := ValidateCopilotPins("", []string{"opus=openai:gpt-5"})
	if err != nil || warn != "" {
		t.Fatalf("warn=%q err=%v, want no-op for a non-copilot pin", warn, err)
	}
}

// A github-copilot pin (provider:model or --provider fallback) only warns when the live catalog is unreachable.
func TestValidateCopilotPinsWarnsWhenCatalogUnreachable(t *testing.T) {
	pinsScratch(t)
	for flag, pin := range map[string]string{"": "opus=personal:claude-opus-5.5", "personal": "opus=claude-opus-5.5"} {
		warn, err := ValidateCopilotPins(flag, []string{pin})
		if err != nil || !strings.Contains(warn, "could not verify") {
			t.Errorf("ValidateCopilotPins(%q, %s) = %q, %v; want the unreachable-catalog warning", flag, pin, warn, err)
		}
	}
}

// doctor's/profile's output over a github-copilot pin or route never prints anything secret-looking:
// no bearer token, no raw API key value, only the env var NAME.
func TestValidateCopilotPinsOutputNamesNoSecret(t *testing.T) {
	pinsScratch(t)
	t.Setenv("GH_TOKEN", "super-secret-token-value")
	warn, _ := ValidateCopilotPins("", []string{"opus=personal:claude-opus-5.5"})
	if strings.Contains(warn, "super-secret-token-value") {
		t.Errorf("warn leaked a credential: %q", warn)
	}
}

// An unverifiable pin (endpoints unreachable) must not stop later pins from being validated.
func TestValidateCopilotPinsChecksPinsAfterUnverifiableOne(t *testing.T) {
	vaulttest.Use(t)
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARNESS_VAULT_KEY", base64.StdEncoding.EncodeToString(key))
	t.Setenv("PATH", t.TempDir())
	t.Setenv("DBX_CLI_TOKEN", "dapi-cli-secret-token")
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"endpoints":[{"name":"chat-a","task":"llm/v1/chat","state":{"ready":"READY"}}]}`))
	}))
	old := http.DefaultClient.Transport
	http.DefaultClient.Transport = srv.Client().Transport
	t.Cleanup(func() { http.DefaultClient.Transport = old; srv.Close() })
	for name, host := range map[string]string{"dbx-down": "https://127.0.0.1:1", "dbx-up": srv.URL} {
		err := providers.SetProviderFields(name, map[string]any{
			"protocol": "anthropic", "preset": "databricks", "host": host,
			"auth": map[string]any{"type": "key", "env": "DBX_CLI_TOKEN"},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err := ValidateCopilotPins("", []string{"opus=dbx-down:chat-a", "sonnet=dbx-up:bogus-id"})
	if err == nil || !strings.Contains(err.Error(), "bogus-id") {
		t.Fatalf("err = %v, want the bogus-id pin rejected after the unreachable one", err)
	}
}

// Only a tools-enabled autonomous run passes --allow-all-tools; either flag alone keeps copilot's own approvals.
func TestHeadlessAutonomousFlags(t *testing.T) {
	for _, c := range []struct{ tools, autonomous, want bool }{{true, true, true}, {true, false, false}, {false, true, false}} {
		argv := copilotArgv(&agent.Headless{Tools: c.tools, Autonomous: c.autonomous})
		if slices.Contains(argv, "--allow-all-tools") != c.want {
			t.Errorf("Tools=%v Autonomous=%v: argv = %v, want --allow-all-tools %v", c.tools, c.autonomous, argv, c.want)
		}
	}
}

// An explicit GitHub target is gated on its own login, not the copilot record's (here pinned to Databricks).
func TestCopilotGateUsesTargetLogin(t *testing.T) {
	pinsScratch(t)
	vaulttest.UseKeyring(t)
	gh := t.TempDir()
	t.Setenv("GH_CONFIG_DIR", gh)
	if err := os.WriteFile(filepath.Join(gh, "hosts.yml"), []byte("github.com:\n    users:\n        work:\n            oauth_token: x\n    user: work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	vaulttest.SetRecord(t, "copilot", "profile", "dev")
	orig := CopilotUsedPct
	CopilotUsedPct = func(_ context.Context, login string) (float64, bool) { return 10, login == "work" }
	t.Cleanup(func() { CopilotUsedPct = orig })
	target := providers.Target{Provider: &providers.Provider{Name: "w", Login: "work", Auth: providers.Auth{Login: "copilot"}}}
	got, notice, err := copilotGate(nil, target)
	if err != nil || notice != "" || got.Provider == nil || got.Provider.Name != "w" {
		t.Fatalf("gate moved an explicit workable GitHub target: got %+v notice %q err %v", got.Provider, notice, err)
	}
}

// An explicit GitHub row's login wins over a stored COPILOT_GITHUB_TOKEN and over the record's pin.
func TestCopilotLaunchTokenForTargetLogin(t *testing.T) {
	pinsScratch(t)
	vaulttest.UseKeyring(t)
	gh := t.TempDir()
	t.Setenv("GH_CONFIG_DIR", gh)
	if err := os.WriteFile(filepath.Join(gh, "hosts.yml"), []byte("github.com:\n    users:\n        work:\n            oauth_token: x\n    user: work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	vaulttest.Secret(t, providers.CopilotTokenEnv, "stored-expired")
	if a, ok := authSourceFor("work"); !ok || a.Account != "work" || a.token == "stored-expired" {
		t.Fatalf("target login lost to the stored token: %+v ok=%v", a, ok)
	}
	if _, err := CopilotLaunchTokenFor("nobody"); err == nil {
		t.Fatal("a target login missing from gh must fail, never fall back to the stored token")
	}
}

// An interactive launch gets the built-in --allow-all, then --reasoning-effort/--context; a headless one neither.
func TestRenderSettingsInteractivePins(t *testing.T) {
	for _, c := range settingsCases {
		t.Run(c.name, func(t *testing.T) {
			want := []string{"run", "--allow-all"}
			if c.effort != "" {
				want = append(want, "--reasoning-effort", c.effort)
			}
			if c.context != "" {
				want = append(want, "--context", c.context)
			}
			for _, headless := range []bool{false, true} {
				s := copilotSettings(t, c.rec, c.flag, headless)
				_, argv, _, cleanup, err := copilotAgent.RenderSettings(t.Context(), s, nil, []string{"run"})
				cleanup()
				w := want
				if headless {
					w = []string{"run"}
				}
				if err != nil || !slices.Equal(argv, w) {
					t.Errorf("headless=%v: argv = %q, %v; want %q", headless, argv, err, w)
				}
			}
		})
	}
}

// The registry launch (LaunchSpec prefix, then the registry argv, as registryPlan joins them) carries each pin once.
func TestRegistryLaunchArgvPinsOnce(t *testing.T) {
	copilotSettings(t, &vault.AgentRecord{Effort: "high", Context: "long_context"}, "", false)
	setGOOS(t, "linux")
	t.Setenv("COPILOT_HOME", t.TempDir())
	stubSettingsRegistry(t)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	cr, err := ConfigureCopilotRegistry(t.Context(), reg, providers.Target{Provider: &providers.Provider{Name: "fixture"}}, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	_, prefix := copilotSchema().LaunchSpec()
	argv := slices.Concat(prefix, cr.Argv, cr.Args)
	for _, flag := range []string{"--reasoning-effort", "--context", "--model"} {
		if n := countOf(argv, flag); n != 1 {
			t.Errorf("argv %q has %s %d times, want once", argv, flag, n)
		}
	}
}

func countOf(argv []string, s string) (n int) {
	for _, a := range argv {
		if a == s {
			n++
		}
	}
	return n
}
