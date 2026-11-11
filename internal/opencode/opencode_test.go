package opencode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent/agenttest"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

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
	t.Setenv(opencodeAPIKeyEnv, "")
	writeAuthJSON(t, data, `{"openai":{"type":"oauth"}}`)
	if l := listOpencodeLogins(); len(l) != 1 || l[0] != "openai" {
		t.Fatalf("precondition: logins = %v, want only openai", l)
	}
	env, err := opencodeCredential("opencode-go")
	if err == nil {
		t.Fatalf("env = %v, want an error: the openai login must not cover an opencode-go cell", env)
	}
	if !strings.Contains(err.Error(), opencodeAPIKeyEnv) {
		t.Errorf("err = %v, want it to name %s", err, opencodeAPIKeyEnv)
	}
}

// TestOpencodeCredentialVaultKeyOutranksMissingOwnLogin checks the same opencode-go cell resolves once the
// OPENCODE_API_KEY vault key is present, still with no "opencode" login stored.
func TestOpencodeCredentialVaultKeyOutranksMissingOwnLogin(t *testing.T) {
	data, _ := fakeOpencode(t, "1.19.3", "", nil)
	writeAuthJSON(t, data, `{"openai":{"type":"oauth"}}`)
	t.Setenv(opencodeAPIKeyEnv, "sk-test-key")
	env, err := opencodeCredential("opencode-go")
	if err != nil {
		t.Fatalf("err = %v, want the vault/env key to resolve it", err)
	}
	if len(env) != 1 || env[0] != opencodeAPIKeyEnv+"=sk-test-key" {
		t.Errorf("env = %v, want [%s=sk-test-key]", env, opencodeAPIKeyEnv)
	}
}

func copilotOnlyOverlayRegistry(t *testing.T) *providers.Registry {
	t.Helper()
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
	return reg
}

// A copilot-only opencode launch renders github-copilot/<id> models, never an opencode-go id.
func TestOpencodeCopilotOnlyOverlayModels(t *testing.T) {
	reg := copilotOnlyOverlayRegistry(t)
	main, small, ok := opencodeCopilotModelIDs(reg)
	if !ok {
		t.Fatal("no copilot model ids")
	}
	_, body, _, _, err := agentconfig.OpencodeOverlayRendered()
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	t.Run("model ids", func(t *testing.T) {
		if doc["model"] != "github-copilot/"+main || doc["small_model"] != "github-copilot/"+small {
			t.Errorf("model=%v small_model=%v, want github-copilot/%s and github-copilot/%s", doc["model"], doc["small_model"], main, small)
		}
	})
}

// TestOpencodeOverlayModelsFollowTable checks the opencode overlay's model/small_model are the table's opencode cells,
// with a vault route override reaching only the overridden cell.
func TestOpencodeOverlayModelsFollowTable(t *testing.T) {
	isolateOpencodeRouteEnv(t) // the host's copilot-only login would swap in github-copilot/ ids
	vaulttest.Use(t)
	render := func() map[string]any {
		_, body, _, _, err := agentconfig.OpencodeOverlayRendered()
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(body, &doc); err != nil {
			t.Fatal(err)
		}
		return doc
	}
	tb, err := router.Load()
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

// TestOpencodeOverlayModelsCorruptProviderOverlay: a provider overlay providers.Load rejects falls back to the table.
func TestOpencodeOverlayModelsCorruptProviderOverlay(t *testing.T) {
	data, _ := isolateOpencodeRouteEnv(t)
	vaulttest.Use(t)
	if err := os.MkdirAll(filepath.Join(data, "opencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "opencode", "auth.json"), []byte(`{"github-copilot":{"type":"oauth"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	vaulttest.Put(t, vault.Overlay, "providers", `{not json`)
	if _, err := providers.Load(); err == nil {
		t.Fatal("fixture must make providers.Load fail")
	}
	if OpencodeOwnLoginPresent() || !OpencodeCopilotLoginPresent() {
		t.Fatal("fixture must be copilot-only")
	}
	wantMain, wantSmall, wantOK := router.OpencodeTableModels()
	main, small, ok := opencodeOverlayModels()
	if main != wantMain || small != wantSmall || ok != wantOK {
		t.Errorf("got %q %q %v, want table %q %q %v", main, small, ok, wantMain, wantSmall, wantOK)
	}
}

// Text already parsed survives a later typed-decode failure.
func TestParseOpenCodeEventsDecodeErrorKeepsText(t *testing.T) {
	stream := `{"type":"text","sessionID":"s-1","part":{"text":"hi"}}
{"type":123}`
	res, err := parseOpenCodeEvents(bufio.NewReader(strings.NewReader(stream)), wire.Discard, nil, nil, nil)
	if err == nil || res.Answer != "hi" || res.SessionID != "s-1" {
		t.Fatalf("res = %+v, err = %v; want the decode error with text and session kept", res, err)
	}
}

// TestOpencodeLaunchEnvIncludesVaultCredential pins the interactive own-login route's vault-only credential.
func TestOpencodeLaunchEnvIncludesVaultCredential(t *testing.T) {
	fakeOpencode(t, "1.19.3", "", nil)
	vaulttest.Use(t)
	vaulttest.Secret(t, opencodeAPIKeyEnv, "sk-vault-only")
	t.Setenv(opencodeAPIKeyEnv, "")
	env := (opencodeEngine{}).LaunchEnv()
	if !slices.Contains(env, opencodeAPIKeyEnv+"=sk-vault-only") {
		t.Fatalf("LaunchEnv lacks the vault credential: %v", env)
	}
}

// TestOpencodeLaunchEnvReportsErrors: no login and no key stays quiet; an unreadable login store and a failed
// overlay write each print one notice line.
func TestOpencodeLaunchEnvReportsErrors(t *testing.T) {
	for _, tc := range []struct {
		name, version string
		listErr       error
		want          []string
	}{
		{"missing credential is silent", "1.19.3", nil, []string{"opencode.json"}},
		{"unreadable login store is reported", "2.0.0", errors.New("store locked"), []string{"could not be read", "opencode.json"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeOpencode(t, tc.version, "", tc.listErr)
			vaulttest.Use(t)
			t.Setenv(opencodeAPIKeyEnv, "")
			if err := os.MkdirAll(filepath.Join(paths.StateDir(), "opencode", "opencode.json"), 0o700); err != nil {
				t.Fatal(err)
			}
			var notice strings.Builder
			orig := agent.NoticeOut
			agent.NoticeOut = &notice
			t.Cleanup(func() { agent.NoticeOut = orig })
			(opencodeEngine{}).LaunchEnv()
			lines := strings.Split(strings.TrimSpace(notice.String()), "\n")
			if len(lines) != len(tc.want) {
				t.Fatalf("notices = %q, want %d line(s) naming %v", notice.String(), len(tc.want), tc.want)
			}
			for i, w := range tc.want {
				if !strings.Contains(lines[i], w) {
					t.Errorf("notice %d = %q, want it to name %q", i, lines[i], w)
				}
			}
		})
	}
}

// TestOpencodeOwnLoginMemoKeepsUntilInvalidate: a vault key added behind the memo stays unseen within the keep,
// and InvalidateOpencodeAuth forces a fresh probe that sees it.
func TestOpencodeOwnLoginMemoKeepsUntilInvalidate(t *testing.T) {
	fakeOpencode(t, "1.19.3", "", nil)
	vaulttest.Use(t)
	t.Setenv(opencodeAPIKeyEnv, "")
	InvalidateOpencodeAuth()
	if OpencodeOwnLoginPresent() {
		t.Fatal("precondition: no login and no key must read as absent")
	}
	vaulttest.Secret(t, opencodeAPIKeyEnv, "placeholder")
	if OpencodeOwnLoginPresent() {
		t.Error("a second call within the keep re-probed the vault; want the remembered answer")
	}
	InvalidateOpencodeAuth()
	if !OpencodeOwnLoginPresent() {
		t.Error("after InvalidateOpencodeAuth the stale answer was kept; want a fresh probe that finds the key")
	}
}

// TestOpencodeRenderSettingsAppliesEffortAndPolicy: effort becomes --variant on `run` only, a policy becomes
// --auto plus OPENCODE_PERMISSION; empty settings leave the run argv as built.
func TestOpencodeRenderSettingsAppliesEffortAndPolicy(t *testing.T) {
	fakeOpencode(t, "1.19.3", "", nil)
	vaulttest.Use(t)
	t.Setenv(opencodeAPIKeyEnv, "")
	perms, err := agent.ParsePermissions("", nil, nil, []string{"Bash(rm:*)"})
	if err != nil {
		t.Fatal(err)
	}
	e := opencodeEngine{agent.NewBaseEngine("opencode")}
	run := []string{"run", "--format", "json", "hi"}
	render := func(s agent.Settings, argv []string) ([]string, []string) {
		t.Helper()
		env, out, _, _, err := e.RenderSettings(context.Background(), s, nil, argv)
		if err != nil {
			t.Fatal(err)
		}
		return env, out
	}
	if _, got := render(agent.Settings{Agent: "opencode", Headless: true}, run); !slices.Equal(got, run) {
		t.Errorf("no settings: argv = %q, want %q unchanged", got, run)
	}
	env, got := render(agent.Settings{Agent: "opencode", Headless: true, Effort: "high", Tools: true, Permissions: perms}, run)
	if want := []string{"run", "--auto", "--variant", "high", "--format", "json", "hi"}; !slices.Equal(got, want) {
		t.Errorf("argv = %q, want %q", got, want)
	}
	if !slices.ContainsFunc(env, func(kv string) bool {
		return strings.HasPrefix(kv, opencodePermissionEnv+"=") && strings.Contains(kv, `"rm *":"deny"`)
	}) {
		t.Errorf("env = %q, want %s carrying the rm deny", env, opencodePermissionEnv)
	}
	if _, got := render(agent.Settings{Agent: "opencode", Effort: "high"}, nil); len(got) != 0 {
		t.Errorf("interactive argv = %q, want no --variant (the TUI has no such flag)", got)
	}
}

// TestOpencodeToolsOffPinsPermission pins the env override an inherited OPENCODE_PERMISSION=allow would otherwise win.
func TestOpencodeToolsOffPinsPermission(t *testing.T) {
	t.Setenv(opencodePermissionEnv, `"allow"`)
	p := &providers.Provider{Name: "keyed"}
	h := &agent.Headless{Prompt: "Run pwd", Tools: false}
	env, _, err := opencode(p, providers.ProtocolOpenAI, "https://example.test/v1", "m", "secret", false, h)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(env, opencodePermissionEnv+`="deny"`) {
		t.Fatalf("tools-off env lacks the deny pin: %v", env)
	}
}

// TestOpencodeChildMarksHostedRuns: a hook host adds the hosted marker; none (memory child) leaves env and argv alone.
func TestOpencodeChildMarksHostedRuns(t *testing.T) {
	bin, argv, dir := "/bin/echo", []string{"run", "hi"}, t.TempDir()
	req := agent.RunOptions{WorkDir: dir, SafeMode: true}
	cmd, _, host, err := opencodeChild(context.Background(), "opencode", bin, []string{"A=1"}, argv, "hi", req)
	if err != nil || host == nil {
		t.Fatalf("opencodeChild = host %v, err %v; want a safety-gate host", host, err)
	}
	if !slices.Contains(cmd.Env, hostedEnv+"=1") || cmd.Dir != dir {
		t.Errorf("hosted child dir %q, want the hosted marker in its env and WorkDir", cmd.Dir) // never print cmd.Env: it holds tokens
	}
	t.Setenv(memory.UpdateChildEnv, "1")
	cmd, _, host, err = opencodeChild(context.Background(), "opencode", bin, []string{"A=1"}, argv, "hi", req)
	if err != nil || host != nil || slices.Contains(cmd.Env, hostedEnv+"=1") || !slices.Equal(cmd.Args[1:], argv) {
		t.Errorf("memory child: host %v err %v args %v; want no host, no marker, argv as given", host, err, cmd.Args)
	}
}

// isolateOpencodeRouteEnv clears every credential/login surface DescribeRoute's opencode/copilot chain
// reads, so a test controls precisely which dispatch (if any) resolves, never a real machine's own key.
func isolateOpencodeRouteEnv(t *testing.T) (dataHome, cfgHome string) {
	t.Helper()
	dataHome, cfgHome = t.TempDir(), t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv("XDG_CONFIG_HOME", cfgHome)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", t.TempDir())          // no real `gh`: CopilotLoginPresent's shell-out dispatch can't fire by accident
	t.Setenv("COPILOT_HOME", t.TempDir())  // no real copilot config.json: copilotStoredLogin reads nothing
	t.Setenv("GH_CONFIG_DIR", t.TempDir()) // no real gh hosts.yml: GHActiveAccount reads nothing
	for _, k := range []string{"ANTHROPIC_API_KEY", "GEMINI_API_KEY", "GH_TOKEN", "GITHUB_TOKEN", "COPILOT_GITHUB_TOKEN"} {
		t.Setenv(k, "")
	}
	return dataHome, cfgHome
}

// opencodeCopilotModelIDs swaps an "auto" alias for the provider's OpencodeFallback; a concrete id passes through.
func TestOpencodeCopilotModelIDsSwapsAutoForFallback(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	for _, k := range []string{"HARNESS_MODEL", "HARNESS_SONNET_MODEL", "HARNESS_HAIKU_MODEL"} {
		t.Setenv(k, "")
	}
	overlay := `{"providers":{"github":{"personal":{"protocol":"openai","base_url":"https://api.githubcopilot.com",` +
		`"auth":{"type":"cli","login":"copilot"},` +
		`"models":{"opus":"gpt-6.1-sol","sonnet":"claude-sonnet-5.5","haiku":"auto"},` +
		`"opencode_fallback":"mai-code-1.1-flash"}}}}`
	vaulttest.Put(t, vault.Overlay, "providers", overlay)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	main, small, ok := opencodeCopilotModelIDs(reg)
	if !ok {
		t.Fatal("opencodeCopilotModelIDs: ok = false")
	}
	if main != "claude-sonnet-5.5" {
		t.Errorf("main = %q, want the registry's own sonnet id unchanged", main)
	}
	if small != "mai-code-1.1-flash" {
		t.Errorf("small = %q, want the haiku alias's \"auto\" swapped for OpencodeFallback", small)
	}
}

// A pin that resolves the haiku alias to a real (non-"auto") id passes through untouched, even though
// the registry's own haiku default is "auto".
func TestOpencodeCopilotModelIDsLeavesNonAutoPinUnchanged(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	vaulttest.SetRecord(t, "claude", "aliases.haiku", "claude-haiku-4.5")
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	_, small, ok := opencodeCopilotModelIDs(reg)
	if !ok {
		t.Fatal("opencodeCopilotModelIDs: ok = false")
	}
	if small != "claude-haiku-4.5" {
		t.Errorf("small = %q, want the pinned id unchanged (not swapped for OpencodeFallback)", small)
	}
}

func TestInteractiveLaunchRejectsPluginWriteFailure(t *testing.T) {
	menu := []providers.OpencodeModel{{ID: "glm-5-2", Label: "GLM 5.2"}}
	db := regressionDatabricksProfile(t, "databricks-test", menu)
	blocked := filepath.Join(paths.StateDir(), "opencode")
	if err := os.MkdirAll(filepath.Dir(blocked), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []*providers.Provider{{Name: "generic", Provider: "github"}, db} {
		t.Run(p.Name, func(t *testing.T) {
			env, argv, err := opencode(p, providers.ProtocolOpenAI, "https://workspace.example.test", agentconfig.MenuWire(p, "glm-5-2"), "", false, nil)
			if err == nil || !strings.Contains(err.Error(), "opencode plugin:") {
				t.Fatalf("interactive launch must report plugin write failure: env=%v argv=%v err=%v", env, argv, err)
			}
			if env != nil || argv != nil {
				t.Fatalf("failed launch returned usable config: env=%v argv=%v", env, argv)
			}
		})
	}
}

func TestOwnLoginLaunchPreservesModelFlag(t *testing.T) {
	const model = "opencode-go/kimi-k3.1-pro"
	cases := []struct{ name, auth, profile string }{
		{"pre-route", `{"opencode":{"type":"api"}}`, ""},
		{"configure", `{"github-copilot":{"type":"oauth"}}`, "personal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agenttest.LaunchHermetic(t)
			pathstest.FakeVendor(t, "opencode", "")
			dir := filepath.Join(os.Getenv("XDG_DATA_HOME"), "opencode")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(tc.auth), 0o600); err != nil {
				t.Fatal(err)
			}
			plan, err := agent.LaunchPlan(t.Context(), "opencode", agent.LaunchOptions{
				Model: model, Provider: tc.profile, PrintEnv: true,
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if want := []string{"--model", model}; !slices.Equal(plan.Argv, want) {
				t.Errorf("Argv = %v, want %v", plan.Argv, want)
			}
		})
	}
}

func TestPlainPlanAppliesVendorLaunchSpec(t *testing.T) {
	agenttest.LaunchHermetic(t)
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	fake := pathstest.FakeVendor(t, "opencode", "")

	vaulttest.Put(t, vault.Overlay, "agents", `{"agents":{"opencode":{"launch":{"env":["X_TEST=1"],"args":["--flag"]}}}}`)
	// A sign-in subcommand takes the plain plan, the only launch that skips routing.
	plan, err := agent.LaunchPlan(t.Context(), "opencode", agent.LaunchOptions{}, []string{"auth", "arg"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Binary != fake {
		t.Errorf("Binary = %q, want %q", plan.Binary, fake)
	}
	if plan.Set["X_TEST"] != "1" {
		t.Errorf("Set[X_TEST] = %q, want 1", plan.Set["X_TEST"])
	}
	wantArgv := []string{"--flag", "auth", "arg"}
	if !slices.Equal(plan.Argv, wantArgv) {
		t.Errorf("Argv = %v, want %v (spec args prepended)", plan.Argv, wantArgv)
	}

	// No overlay: the built-in default still applies.
	vaulttest.Put(t, vault.Overlay, "agents", "")
	plan, err = agent.LaunchPlan(t.Context(), "opencode", agent.LaunchOptions{}, []string{"auth"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Binary != fake {
		t.Errorf("Binary = %q, want %q", plan.Binary, fake)
	}
	if v, ok := plan.Set["OPENCODE_DISABLE_CLAUDE_CODE_SKILLS"]; ok {
		t.Errorf("Set[OPENCODE_DISABLE_CLAUDE_CODE_SKILLS] = %q, want unset (opencode loads ~/.claude/skills)", v)
	}
	if !slices.Equal(plan.Argv, []string{"auth"}) {
		t.Errorf("Argv = %v, want [auth] (opencode's default carries no Launch.Args)", plan.Argv)
	}
}
