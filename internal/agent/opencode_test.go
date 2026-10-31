package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestOpenCodeConfigShape(t *testing.T) {
	tg := target(t, "gateway", "anthropic/claude-opus-5")
	env, argv, err := schemaFor("opencode").configureHeadless(WithDirect(context.Background()), tg, Headless{Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Model      string                    `json:"model"`
		Permission any                       `json:"permission"`
		Provider   map[string]map[string]any `json:"provider"`
	}
	if err := json.Unmarshal([]byte(envMap(env)["OPENCODE_CONFIG_CONTENT"]), &cfg); err != nil {
		t.Fatal(err)
	}
	prov := cfg.Provider["harness-gateway"]
	opts := prov["options"].(map[string]any)
	if prov["npm"] != "@ai-sdk/anthropic" || opts["baseURL"] != "https://gateway.example.com/api/v1" {
		t.Errorf("provider block = %v", prov)
	}
	if opts["apiKey"] != "{env:"+OpenCodeKeyEnv+"}" || opts["headers"].(map[string]any)["Authorization"] != "Bearer {env:"+OpenCodeKeyEnv+"}" {
		t.Errorf("auth must reference the env var, got %v", opts)
	}
	if cfg.Permission != "deny" || cfg.Model != "harness-gateway/anthropic/claude-opus-5" {
		t.Errorf("tools-off permission/model wrong: %+v", cfg)
	}
	if strings.Contains(envMap(env)["OPENCODE_CONFIG_CONTENT"], secret) || strings.Contains(strings.Join(argv, " "), secret) {
		t.Error("secret inlined into config or argv")
	}
}

// TestOpenCodeDirArgv covers both argv builders opencode's workdir plumbing goes through: the BYOK/gateway
// builder (opencode(), reached via ConfigureHeadless) and the hub ladder's own (OpencodeRunArgv). Both must
// add --dir only when Headless.WorkDir is set, and never otherwise.
func TestOpenCodeDirArgv(t *testing.T) {
	t.Run("BYOK builder", func(t *testing.T) {
		tg := target(t, "gateway", "anthropic/claude-opus-5")
		_, argv, err := schemaFor("opencode").configureHeadless(context.Background(), tg, Headless{Prompt: "hi", WorkDir: "/work/dir"})
		if err != nil {
			t.Fatal(err)
		}
		if i := slices.Index(argv, "--dir"); i == -1 || argv[i+1] != "/work/dir" {
			t.Errorf("argv = %q, want --dir /work/dir", argv)
		}

		_, argv, err = schemaFor("opencode").configureHeadless(context.Background(), tg, Headless{Prompt: "hi"})
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(argv, "--dir") {
			t.Errorf("argv = %q, must not contain --dir when WorkDir is unset", argv)
		}
	})

	t.Run("hub ladder", func(t *testing.T) {
		_, argv := opencodeRunArgv("github-copilot/claude-opus-5", Headless{Prompt: "hi", WorkDir: "/work/dir"})
		if i := slices.Index(argv, "--dir"); i == -1 || argv[i+1] != "/work/dir" {
			t.Errorf("argv = %q, want --dir /work/dir", argv)
		}

		_, argv = opencodeRunArgv("github-copilot/claude-opus-5", Headless{Prompt: "hi"})
		if slices.Contains(argv, "--dir") {
			t.Errorf("argv = %q, must not contain --dir when WorkDir is unset", argv)
		}
	})
}

// TestOpenCodeOverlayEnvOnEveryRoute: applyLaunch wires OPENCODE_CONFIG/OPENCODE_TUI_CONFIG into every
// opencode launch path — own login (LaunchSpec), the BYOK/gateway route (Configure) and the hub ladder's
// own builder (OpencodeRunArgv) — not just the applyLaunch unit the overlay's own test already covers.
func TestOpenCodeOverlayEnvOnEveryRoute(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	hasOverlayEnv := func(t *testing.T, env []string) {
		t.Helper()
		m := envMap(env)
		if m["OPENCODE_CONFIG"] == "" || m["OPENCODE_TUI_CONFIG"] == "" {
			t.Errorf("env = %v, want both OPENCODE_CONFIG and OPENCODE_TUI_CONFIG set", env)
		}
	}

	t.Run("own login (LaunchSpec)", func(t *testing.T) {
		env, _ := schemaFor("opencode").LaunchSpec()
		hasOverlayEnv(t, env)
	})

	t.Run("BYOK/gateway (Configure)", func(t *testing.T) {
		tg := target(t, "gateway", "anthropic/claude-opus-5")
		env, _, err := schemaFor("opencode").configureHeadless(context.Background(), tg, Headless{Prompt: "hi"})
		if err != nil {
			t.Fatal(err)
		}
		hasOverlayEnv(t, env)
	})

	t.Run("hub ladder (OpencodeRunArgv)", func(t *testing.T) {
		env, _ := opencodeRunArgv("github-copilot/claude-opus-5", Headless{Prompt: "hi"})
		hasOverlayEnv(t, env)
	})
}

// TestOpenCodeOwnLoginHeadlessToolsOffDeny: opencode's own-login headless leg (OwnLoginHeadless) still
// rides OPENCODE_CONFIG_CONTENT={"permission":"deny"} when tools are off, alongside the new overlay env.
func TestOpenCodeOwnLoginHeadlessToolsOffDeny(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	env, _, err := schemaFor("opencode").eng().ownLogin(Headless{Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	m := envMap(env)
	if m["OPENCODE_CONFIG_CONTENT"] != `{"permission":"deny"}` {
		t.Errorf("OPENCODE_CONFIG_CONTENT = %q, want the tools-off deny", m["OPENCODE_CONFIG_CONTENT"])
	}
	if m["OPENCODE_CONFIG"] == "" || m["OPENCODE_TUI_CONFIG"] == "" {
		t.Errorf("env = %v, want both overlay paths alongside the deny", env)
	}

	env, _, err = schemaFor("opencode").eng().ownLogin(Headless{Prompt: "hi", Tools: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := envMap(env)["OPENCODE_CONFIG_CONTENT"]; ok {
		t.Errorf("tools=true must carry no OPENCODE_CONFIG_CONTENT deny: %v", env)
	}
}

// opencodeMenu unmarshals OPENCODE_CONFIG_CONTENT and returns just the model menu + the top-level model id.
func opencodeMenu(t *testing.T, env []string, id string) (map[string]any, string) {
	t.Helper()
	var cfg struct {
		Model    string                    `json:"model"`
		Provider map[string]map[string]any `json:"provider"`
	}
	if err := json.Unmarshal([]byte(envMap(env)["OPENCODE_CONFIG_CONTENT"]), &cfg); err != nil {
		t.Fatal(err)
	}
	menu, _ := cfg.Provider[id]["models"].(map[string]any)
	return menu, cfg.Model
}

func TestOpenCodeModelMenu(t *testing.T) {
	t.Run("known tier id", func(t *testing.T) {
		tg := target(t, "anthropic", "claude-sonnet-5-5")
		env, _, err := schemaFor("opencode").Configure(context.Background(), tg, "")
		if err != nil {
			t.Fatal(err)
		}
		menu, _ := opencodeMenu(t, env, "harness-anthropic")
		want := map[string]bool{"claude-opus-5-5": true, "claude-sonnet-5-5": true, "claude-haiku-5-5": true, "claude-fable-5-1": true}
		if len(menu) != len(want) {
			t.Fatalf("menu = %v, want exactly the 4 tier ids", menu)
		}
		for id := range want {
			if _, ok := menu[id]; !ok {
				t.Errorf("menu missing tier id %q: %v", id, menu)
			}
		}
	})

	t.Run("model not in table", func(t *testing.T) {
		tg := target(t, "anthropic", "claude-custom")
		env, _, err := schemaFor("opencode").Configure(context.Background(), tg, "")
		if err != nil {
			t.Fatal(err)
		}
		menu, model := opencodeMenu(t, env, "harness-anthropic")
		if len(menu) != 5 {
			t.Errorf("menu = %v, want 5 keys (4 tiers + the requested model)", menu)
		}
		if model != "harness-anthropic/claude-custom" {
			t.Errorf("model = %q, want harness-anthropic/claude-custom", model)
		}
	})

	t.Run("headless carries no menu", func(t *testing.T) {
		tg := target(t, "anthropic", "claude-sonnet-5-5")
		env, _, err := schemaFor("opencode").configureHeadless(context.Background(), tg, Headless{Prompt: "hi"})
		if err != nil {
			t.Fatal(err)
		}
		menu, _ := opencodeMenu(t, env, "harness-anthropic")
		if len(menu) != 1 {
			t.Errorf("headless menu = %v, want exactly 1 key", menu)
		}
	})
}

func TestMutateOpencodeConfigEnvPolicies(t *testing.T) {
	set := func(c map[string]any) { c["k"] = "v" }
	bad := []string{"A=1", opencodeConfigEnv + "={nope"}
	if _, err := mutateOpencodeConfigEnv(append([]string(nil), bad...), false, set); err == nil {
		t.Error("strict: an unreadable block must be an error")
	}
	got, err := mutateOpencodeConfigEnv(append([]string(nil), bad...), true, set)
	if err != nil || got[1] != opencodeConfigEnv+`={"k":"v"}` || len(got) != 2 {
		t.Errorf("lenient = %v, %v; want the block replaced in place", got, err)
	}
	got, _ = mutateOpencodeConfigEnv([]string{"A=1", opencodeConfigEnv + `={"m":1}`}, false, set)
	if got[1] != opencodeConfigEnv+`={"k":"v","m":1}` {
		t.Errorf("merge = %v", got)
	}
	if got, _ = mutateOpencodeConfigEnv(nil, false, set); len(got) != 1 || got[0] != opencodeConfigEnv+`={"k":"v"}` {
		t.Errorf("absent = %v, want one appended entry", got)
	}
}

// TestOpencodeCredentialFallsThroughWhenOnlyAnotherLoginExists checks that when opencode has a login for
// another provider (openai) but none for its own "opencode" plan, an opencode-go cell (prefix "opencode-go"
// maps to credential id "opencode") falls through to the OPENCODE_API_KEY vault key, erroring when that is
// absent too — precedence never borrows the unrelated openai login.
func TestOpencodeCredentialFallsThroughWhenOnlyAnotherLoginExists(t *testing.T) {
	data, _ := fakeOpencode(t, "1.19.3", "", nil)
	t.Setenv(OpencodeAPIKeyEnv, "")
	writeAuthJSON(t, data, `{"openai":{"type":"oauth"}}`)
	if l := OpencodeLogins(); len(l) != 1 || l[0] != "openai" {
		t.Fatalf("precondition: logins = %v, want only openai", l)
	}
	env, err := opencodeCredential("opencode-go")
	if err == nil {
		t.Fatalf("env = %v, want an error: the openai login must not cover an opencode-go cell", env)
	}
	if !strings.Contains(err.Error(), OpencodeAPIKeyEnv) {
		t.Errorf("err = %v, want it to name %s", err, OpencodeAPIKeyEnv)
	}
}

// TestOpencodeCredentialVaultKeyOutranksMissingOwnLogin checks the same opencode-go cell resolves once the
// OPENCODE_API_KEY vault key is present, still with no "opencode" login stored.
func TestOpencodeCredentialVaultKeyOutranksMissingOwnLogin(t *testing.T) {
	data, _ := fakeOpencode(t, "1.19.3", "", nil)
	writeAuthJSON(t, data, `{"openai":{"type":"oauth"}}`)
	t.Setenv(OpencodeAPIKeyEnv, "sk-test-key")
	env, err := opencodeCredential("opencode-go")
	if err != nil {
		t.Fatalf("err = %v, want the vault/env key to resolve it", err)
	}
	if len(env) != 1 || env[0] != OpencodeAPIKeyEnv+"=sk-test-key" {
		t.Errorf("env = %v, want [%s=sk-test-key]", env, OpencodeAPIKeyEnv)
	}
}

func TestProbeEndpointsClientRouteParity(t *testing.T) {
	for _, tc := range []struct {
		name, id, path, body string
		responses            bool
		anthropic            bool
	}{
		{"chat", "gpt-5-1", "/serving-endpoints/custom/chat/chat/completions",
			`{"messages":[{"role":"user","content":"Reply with OK."}],"max_tokens":256,"tools":[{"type":"function","function":{"name":"ping","description":"Return pong.","parameters":{"type":"object","properties":{}}}}],"tool_choice":"auto"}`, false, false},
		{"native-claude", "claude-sonnet-5-5", "/serving-endpoints/custom/v1/messages",
			`{"messages":[{"role":"user","content":"Reply with OK."}],"max_tokens":32,"tools":[{"name":"ping","description":"Return pong.","input_schema":{"type":"object","properties":{}}}],"tool_choice":{"type":"auto"}}`, false, true},
		{"responses", "gpt-5-5-pro", "/serving-endpoints/custom-responses/responses",
			`{"input":"Reply with OK.","max_output_tokens":128,"tools":[{"type":"function","name":"ping","description":"Return pong.","parameters":{"type":"object","properties":{}}}]}`, true, false},
		{"explicit-responses-before-claude", "claude-sonnet-5-5", "/serving-endpoints/custom-responses/responses",
			`{"input":"Reply with OK.","max_output_tokens":128,"tools":[{"type":"function","name":"ping","description":"Return pong.","parameters":{"type":"object","properties":{}}}]}`, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vaulttest.Use(t)
			t.Setenv("CLIENT_ROUTE_TOKEN", "synthetic-route-token")
			type request struct {
				method, path string
				header       http.Header
				body         map[string]any
			}
			requests := make(chan request, 4)
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("probe body: %v", err)
				}
				requests <- request{r.Method, r.URL.RequestURI(), r.Header.Clone(), body}
				_, _ = w.Write([]byte(`{"ok":true}`))
			}))
			t.Cleanup(srv.Close)
			// Exercise both the primary BaseURL and relative/absolute p.Route overrides.
			p := &providers.Provider{Name: "client-routes", Preset: providers.PresetDatabricks, Host: srv.URL,
				Protocol: providers.ProtocolAnthropic, BaseURL: srv.URL + "/serving-endpoints/custom",
				Routes: map[string]string{providers.ProtocolOpenAI: "chat", providers.ProtocolResponses: srv.URL + "/serving-endpoints/custom-responses"},
				Auth:   providers.Auth{Type: providers.AuthKey, Env: "CLIENT_ROUTE_TOKEN"}}
			target := CheckTarget{ID: tc.id, Wire: "databricks-" + tc.id, Responses: tc.responses}
			got, err := ProbeEndpoints(context.Background(), p, []CheckTarget{target}, CheckOptions{Client: srv.Client(), ClientRoutes: true})
			if err != nil || len(got) != 1 || got[0].Status != "ok" || got[0].HTTP != 200 || got[0].Tools != "yes" {
				t.Fatalf("ProbeEndpoints = %+v, %v", got, err)
			}
			if len(requests) != 1 {
				t.Fatalf("requests = %d, want one", len(requests))
			}
			r := <-requests
			if r.method != "POST" || r.path != tc.path || r.header.Get("Content-Type") != "application/json" || r.header.Get("Authorization") != "Bearer synthetic-route-token" {
				t.Errorf("wrong method/route/headers: %s %s", r.method, r.path)
			}
			version := ""
			if tc.anthropic {
				version = "2023-06-01"
			}
			if r.header.Get("anthropic-version") != version {
				t.Errorf("anthropic-version = %q, want %q", r.header.Get("anthropic-version"), version)
			}
			var want map[string]any
			if err := json.Unmarshal([]byte(tc.body), &want); err != nil {
				t.Fatal(err)
			}
			want["model"] = target.Wire
			if !reflect.DeepEqual(r.body, want) {
				t.Errorf("client body = %#v, want %#v", r.body, want)
			}
		})
	}
}

func TestProbeEndpointsClientRoutesRejectUnsafeDestinations(t *testing.T) {
	vaulttest.Use(t)
	t.Setenv("CLIENT_ROUTE_TOKEN", "synthetic-route-token")
	var calls, redirected atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/redirected" {
			redirected.Add(1)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		http.Redirect(w, r, "/redirected", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(srv.Close)
	p := providers.Provider{Name: "safe-routes", Preset: providers.PresetDatabricks, Host: srv.URL,
		Protocol: providers.ProtocolOpenAI, BaseURL: srv.URL + "/serving-endpoints",
		Auth: providers.Auth{Type: providers.AuthKey, Env: "CLIENT_ROUTE_TOKEN"}}
	targets := []CheckTarget{{ID: "glm-5-2", Wire: "databricks-glm-5-2"}}
	for _, tc := range []struct {
		name string
		edit func(*providers.Provider)
	}{
		{"gateway", func(p *providers.Provider) { p.Options.Wire = providers.WireAIGateway }},
		{"off-workspace", func(p *providers.Provider) { p.BaseURL = "https://127.0.0.1:1/serving-endpoints" }},
		{"outside-serving", func(p *providers.Provider) { p.BaseURL = srv.URL + "/ai-gateway" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			other := p
			tc.edit(&other)
			// Reject every transport call: these guards must run before sending credentials.
			client := &http.Client{Transport: discoveryTransport(func(*http.Request) (*http.Response, error) {
				t.Error("invalid route reached the HTTP transport")
				return nil, context.Canceled
			})}
			if _, err := ProbeEndpoints(context.Background(), &other, targets, CheckOptions{Client: client, ClientRoutes: true}); err == nil {
				t.Fatal("unsafe route accepted")
			}
		})
	}
	got, err := ProbeEndpoints(context.Background(), &p, targets, CheckOptions{Client: srv.Client(), ClientRoutes: true})
	if err != nil || len(got) != 1 || got[0].Status != "fail" || got[0].HTTP != 307 || calls.Load() != 2 || redirected.Load() != 0 {
		t.Fatalf("redirect must fail both tool/no-tool attempts without following: results=%+v err=%v calls=%d redirected=%d", got, err, calls.Load(), redirected.Load())
	}
}

// TestOpencodeOverlayModelComesFromTable checks the overlay's model/small_model are the routing table's opencode models.
func TestOpencodeOverlayModelComesFromTable(t *testing.T) {
	isolateOpencodeRouteEnv(t)
	vaulttest.Use(t)
	wantMain, wantSmall, ok := OpencodeTableModels()
	if !ok {
		t.Fatal("routing table has no opencode sonnet/haiku models")
	}
	_, cfgBody, _, _, err := OpencodeOverlayRendered()
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(cfgBody, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["model"] != wantMain || doc["small_model"] != wantSmall {
		t.Errorf("model=%v small_model=%v, want the table's %s and %s", doc["model"], doc["small_model"], wantMain, wantSmall)
	}
}

// TestLaunchEnvCarriesOverlayPaths checks every opencode launch (applyLaunch) carries both overlay env vars.
func TestLaunchEnvCarriesOverlayPaths(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	env, _ := schemaFor("opencode").applyLaunch(nil, nil, true)
	var haveCfg, haveTUI bool
	for _, kv := range env {
		switch {
		case len(kv) > len("OPENCODE_CONFIG=") && kv[:len("OPENCODE_CONFIG=")] == "OPENCODE_CONFIG=":
			haveCfg = true
		case len(kv) > len("OPENCODE_TUI_CONFIG=") && kv[:len("OPENCODE_TUI_CONFIG=")] == "OPENCODE_TUI_CONFIG=":
			haveTUI = true
		}
	}
	if !haveCfg || !haveTUI {
		t.Errorf("applyLaunch(opencode) env = %v, want both OPENCODE_CONFIG and OPENCODE_TUI_CONFIG", env)
	}
}

// A copilot-only opencode launch renders github-copilot/<id> models, never an opencode-go id.
func TestOpencodeCopilotOnlyOverlayModels(t *testing.T) {
	data, _ := isolateOpencodeRouteEnv(t)
	vaulttest.Use(t)
	if err := os.MkdirAll(filepath.Join(data, "opencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "opencode", "auth.json"), []byte(`{"github-copilot":{"type":"oauth"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"github":{"personal":{"protocol":"openai",`+
		`"base_url":"https://api.githubcopilot.com","auth":{"type":"cli","login":"copilot"},`+
		`"models":{"opus":"gpt-6.1-sol","sonnet":"claude-sonnet-5.5","haiku":"mai-code-1.1-flash"}}}}}`)
	if OpencodeOwnLoginPresent() || !OpencodeCopilotLoginPresent() {
		t.Fatal("fixture must be copilot-only")
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	main, small, ok := OpencodeCopilotModelIDs(reg)
	if !ok {
		t.Fatal("no copilot model ids")
	}
	_, body, _, _, err := OpencodeOverlayRendered()
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["model"] != "github-copilot/"+main || doc["small_model"] != "github-copilot/"+small {
		t.Errorf("model=%v small_model=%v, want github-copilot/%s and github-copilot/%s", doc["model"], doc["small_model"], main, small)
	}
	for _, k := range []string{"model", "small_model"} {
		if s, _ := doc[k].(string); strings.HasPrefix(s, "opencode-go/") {
			t.Errorf("%s = %q, an opencode-go id on a copilot-only login", k, s)
		}
	}
}

// TestOpencodeOverlayModelsFollowTable checks the opencode overlay's model/small_model are the table's opencode cells,
// with a vault route override reaching only the overridden cell.
func TestOpencodeOverlayModelsFollowTable(t *testing.T) {
	vaulttest.Use(t)
	render := func() map[string]any {
		_, body, _, _, err := OpencodeOverlayRendered()
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(body, &doc); err != nil {
			t.Fatal(err)
		}
		return doc
	}
	tb, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	sonnet, _ := tb.Cell("opencode", "sonnet")
	haiku, _ := tb.Cell("opencode", "haiku")
	if doc := render(); doc["model"] != sonnet.Model || doc["small_model"] != haiku.Model {
		t.Errorf("overlay = %v/%v, want the table's %q/%q", doc["model"], doc["small_model"], sonnet.Model, haiku.Model)
	}
	vaulttest.Put(t, vault.Overlay, "providers", `{"routing":{"cells":{"opencode":{"sonnet":{"model":"github-copilot/overridden-model"}}}}}`)
	if doc := render(); doc["model"] != "github-copilot/overridden-model" || doc["small_model"] != haiku.Model {
		t.Errorf("overridden overlay = %v/%v, want github-copilot/overridden-model and unchanged %q", doc["model"], doc["small_model"], haiku.Model)
	}
}
