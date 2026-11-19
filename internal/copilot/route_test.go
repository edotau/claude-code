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
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

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
			_, err := validateDatabricksCopilotPin(t.Context(), &providers.Registry{}, "dev", tc.model, served)
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
	ids, err := copilotModelIDs(t.Context())
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
	hosttest.PinScratch(t)
	warn, err := ValidateCopilotPins(t.Context(), "", []string{"opus=openai:gpt-5"})
	if err != nil || warn != "" {
		t.Fatalf("warn=%q err=%v, want no-op for a non-copilot pin", warn, err)
	}
}

// A github-copilot pin (provider:model or --provider fallback) only warns when the live catalog is unreachable.
func TestValidateCopilotPinsWarnsWhenCatalogUnreachable(t *testing.T) {
	hosttest.PinScratch(t)
	for flag, pin := range map[string]string{"": "opus=copilot:claude-opus-5.5", "copilot": "opus=claude-opus-5.5"} {
		warn, err := ValidateCopilotPins(t.Context(), flag, []string{pin})
		if err != nil || !strings.Contains(warn, "could not verify") {
			t.Errorf("ValidateCopilotPins(%q, %s) = %q, %v; want the unreachable-catalog warning", flag, pin, warn, err)
		}
	}
}

// Doctor and profile output over a github-copilot pin or route never prints a token or key, only its env name.
func TestValidateCopilotPinsOutputNamesNoSecret(t *testing.T) {
	hosttest.PinScratch(t)
	t.Setenv("GH_TOKEN", "super-secret-token-value")
	warn, _ := ValidateCopilotPins(t.Context(), "", []string{"opus=copilot:claude-opus-5.5"})
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
	_, err := ValidateCopilotPins(t.Context(), "", []string{"opus=dbx-down:chat-a", "sonnet=dbx-up:bogus-id"})
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
	hosttest.PinScratch(t)
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
	got, notice, err := copilotGate(t.Context(), nil, target, false, "")
	if err != nil || notice != "" || got.Provider == nil || got.Provider.Name != "w" {
		t.Fatalf("gate moved an explicit workable GitHub target: got %+v notice %q err %v", got.Provider, notice, err)
	}
}

// An explicit GitHub row's login wins over a stored COPILOT_GITHUB_TOKEN and over the record's pin.
func TestCopilotLaunchTokenForTargetLogin(t *testing.T) {
	hosttest.PinScratch(t)
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
	if _, err := copilotLaunchTokenFor("nobody"); err == nil {
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
	// a bare Linux box (no login session) has no /run/user/<uid>
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	stubSettingsRegistry(t)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	cr, err := configureCopilotRegistry(t.Context(), reg, providers.Target{Provider: &providers.Provider{Name: "fixture"}}, nil, true)
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

// macFallbackScratch is a hermetic macOS host with no GitHub login and one keyed Databricks profile, dbx.
func macFallbackScratch(t *testing.T) {
	t.Helper()
	hosttest.PinScratch(t)
	vaulttest.UseKeyring(t)
	setGOOS(t, "darwin")
	for _, k := range []string{"GH_TOKEN", "GITHUB_TOKEN", providers.CopilotTokenEnv} {
		t.Setenv(k, "")
	}
	t.Setenv("GH_CONFIG_DIR", t.TempDir())
	t.Setenv("COPILOT_HOME", t.TempDir())
	t.Setenv("DBX_CLI_TOKEN", "dapi-fixture")
	err := providers.SetProviderFields("dbx", map[string]any{"protocol": "anthropic", "preset": "databricks",
		"host": "https://127.0.0.1:1", "auth": map[string]any{"type": "key", "env": "DBX_CLI_TOKEN"}})
	if err != nil {
		t.Fatal(err)
	}
}

// A Databricks id the refused GitHub login hands to the fallback profile runs only if that profile serves it.
func TestCopilotGateRefusesModelFallbackProfileDoesNotServe(t *testing.T) {
	macFallbackScratch(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"routing":{"servedAware":true},"providers":{"databricks":{"dbx":`+
		`{"preset":"databricks","host":"https://127.0.0.1:1","auth":{"type":"key","env":"DBX_CLI_TOKEN"}}}}}`)
	if err := router.SaveServed(router.Served{Profile: "dbx", FetchedAt: time.Now(), Models: []router.ServedModel{
		{ID: "databricks-kimi-k3", Ready: "READY"}, {ID: "databricks-glm-5-3", Ready: "NOT_READY"}}}); err != nil {
		t.Fatal(err)
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	gh := &providers.Provider{Name: "copilot", Auth: providers.Auth{Login: "copilot"}}
	if got, _, err := copilotGate(t.Context(), reg, providers.Target{Provider: gh, Model: "databricks-kimi-k3"}, false, ""); err != nil ||
		got.Provider == nil || got.Provider.Name != "dbx" {
		t.Fatalf("served kimi: gate = %+v, %v; want the dbx fallback", got.Provider, err)
	}
	_, _, err = copilotGate(t.Context(), reg, providers.Target{Provider: gh, Model: "databricks-glm-5-3"}, false, "")
	if err == nil || !strings.Contains(err.Error(), "dbx serves no databricks-glm-5-3") {
		t.Fatalf("unserved glm: gate err = %v, want a refusal naming dbx and databricks-glm-5-3", err)
	}
}

// Inside databricks:dbx a refused GitHub hop is an error naming why; it never moves the dispatch to a workspace.
func TestArmCopilotGateRefusalIsError(t *testing.T) {
	macFallbackScratch(t)
	t.Setenv(agentconfig.OverlayVar, `{"provider":"databricks","profile":"dbx"}`)
	orig := CopilotUsedPct
	CopilotUsedPct = func(context.Context, string) (float64, bool) { return 99, true }
	t.Cleanup(func() { CopilotUsedPct = orig })
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	gh := &providers.Provider{Name: "free", Login: "free", Auth: providers.Auth{Login: "copilot"}}
	got, notice, err := copilotGate(t.Context(), reg, providers.Target{Provider: gh}, false, "")
	if err == nil || got.Provider != nil || notice != "" || !strings.Contains(err.Error(), "inside databricks:dbx") {
		t.Fatalf("armed gate = %+v, %q, %v; want an error naming databricks:dbx, no Databricks move",
			got.Provider, notice, err)
	}
}

// A typed GitHub account with no credits is refused under none, takes the pin under pin, never moves to Databricks.
func TestCopilotGateExplicitAccountNoCredits(t *testing.T) {
	macFallbackScratch(t)
	orig := CopilotUsedPct
	CopilotUsedPct = func(_ context.Context, login string) (float64, bool) { return 10, login == "biz" }
	t.Cleanup(func() { CopilotUsedPct = orig })
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"github":{`+
		`"free":{"protocol":"openai","base_url":"https://api.githubcopilot.com","login":"free","auth":{"type":"cli","login":"copilot"}},`+
		`"biz":{"protocol":"openai","base_url":"https://api.githubcopilot.com","login":"biz","auth":{"type":"cli","login":"copilot"}}}}}`)
	vaulttest.SetRecord(t, "copilot", "profile", "biz")
	hosts := "github.com:\n    users:\n        biz:\n            oauth_token: x\n    user: biz\n"
	if err := os.WriteFile(filepath.Join(os.Getenv("GH_CONFIG_DIR"), "hosts.yml"), []byte(hosts), 0o600); err != nil {
		t.Fatal(err)
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	typed := providers.Target{Provider: reg.Providers["free"], Model: "claude-opus-5.5"}
	if _, _, err := copilotGate(t.Context(), reg, typed, true, "none"); err == nil || !strings.Contains(err.Error(), "named explicitly") {
		t.Fatalf("none: err = %v, want a refusal naming the explicit account", err)
	}
	got, note, err := copilotGate(t.Context(), reg, typed, true, "pin")
	if err != nil || got.Provider.Name != "biz" || !strings.Contains(note, "free") || !strings.Contains(note, "biz") {
		t.Fatalf("pin: got %+v note %q err %v, want the business pin and a note naming both", got.Provider, note, err)
	}
}

func TestModelEffort(t *testing.T) {
	old := effortModels
	t.Cleanup(func() { effortModels = old })
	effortModels = func() []SDKModel {
		return []SDKModel{{ID: "supported", Efforts: []string{"high", "low"}}, {ID: "gpt-4.1", Efforts: []string{}}, {ID: "nil", ReasoningEffort: true},
			{ID: "plain"}}
	}
	served := &providers.Provider{Name: "dev", Preset: providers.PresetDatabricks}
	for _, c := range []struct {
		model, effort, want, note, argv string
		p                               *providers.Provider
	}{
		{"supported", "high", "high", "", "--reasoning-effort high", nil},
		{"supported", "medium", "low", "effort medium→low", "--reasoning-effort low", nil},
		{"gpt-4.1", "medium", "", "copilot: gpt-4.1 takes no reasoning effort; not sent", "", nil},
		{"gpt-4.1[1m]", "medium", "", "copilot: gpt-4.1[1m] takes no reasoning effort; not sent", "", nil},
		{"unknown", "medium", "", "copilot: unknown takes no reasoning effort; not sent", "", nil},
		{"auto", "medium", "", "copilot: auto takes no reasoning effort; not sent", "", nil},
		{"plain", "medium", "", "copilot: plain takes no reasoning effort; not sent", "", nil},
		{"nil", "medium", "medium", "", "--reasoning-effort medium", nil},
		{"databricks-gemini-4-argon", "medium", "", "copilot: databricks-gemini-4-argon takes no reasoning effort; not sent", "", served},
		{"databricks-gpt-6-2-sol", "xhigh", "xhigh", "", "--reasoning-effort xhigh", served},
		{"databricks-gpt-oss-120b", "max", "high", "effort max→high", "--reasoning-effort high", served},
	} {
		t.Run(c.model+"/"+c.effort, func(t *testing.T) {
			s := agent.Settings{Effort: c.effort, Target: providers.Target{Provider: c.p, Model: c.model}}
			_, argv, notes, cleanup, err := copilotAgent.RenderSettings(t.Context(), s, nil, nil)
			cleanup()
			if err != nil || strings.Join(argv, " ") != c.argv {
				t.Errorf("argv = %q, err = %v", argv, err)
			}
			if strings.Join(notes, "") != c.note {
				t.Errorf("notes = %q; want %q", notes, c.note)
			}
			opts, err := newSDKOptions("copilot", nil, agent.RunOptions{WorkDir: t.TempDir()}, s)
			if strings.Join(opts.EffortNotes, "") != c.note {
				t.Errorf("SDK notes = %q; want %q", opts.EffortNotes, c.note)
			}
			if err != nil || opts.ReasoningEffort != c.want {
				t.Errorf("SDK effort = %q, err = %v; want %q", opts.ReasoningEffort, err, c.want)
			}
			// a model id the runtime knows makes it send its own default effort
			if id := runtimeModelID("known", c.model); c.p != nil && (id == "known") != (c.want != "") {
				t.Errorf("runtime model id = %q with effort %q", id, c.want)
			}
		})
	}
}

func TestCopilotArgvFlagsAreDeclared(t *testing.T) {
	copilotSettings(t, &vault.AgentRecord{Effort: "high", Context: "long_context"}, "", false)
	setGOOS(t, "linux")
	t.Setenv("COPILOT_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	stubSettingsRegistry(t)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	cr, err := configureCopilotRegistry(t.Context(), reg, providers.Target{Provider: &providers.Provider{Name: "fixture"}}, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	_, prefix := copilotSchema().LaunchSpec()
	for _, h := range []*agent.Headless{nil, {Prompt: "prompt", Tools: true, Autonomous: true}} {
		argv := slices.Concat(prefix, cr.Argv, cr.Args, copilotArgv(h))
		if got := copilotSchema().CLI.Undeclared(argv); len(got) > 0 {
			t.Errorf("copilot passes %v, not in its row's Flags", got)
		}
	}
}
