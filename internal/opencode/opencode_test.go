package opencode

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
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

// TestOpencodeOverlayModelComesFromTable checks the overlay's model/small_model are the routing table's opencode models.
func TestOpencodeOverlayModelComesFromTable(t *testing.T) {
	isolateOpencodeRouteEnv(t)
	vaulttest.Use(t)
	wantMain, wantSmall, ok := router.OpencodeTableModels()
	if !ok {
		t.Fatal("routing table has no opencode sonnet/haiku models")
	}
	_, cfgBody, _, _, err := agentconfig.OpencodeOverlayRendered()
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
	main, small, ok := OpencodeCopilotModelIDs(reg)
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
	vaulttest.Secret(t, OpencodeAPIKeyEnv, "sk-vault-only")
	t.Setenv(OpencodeAPIKeyEnv, "")
	env := (opencodeEngine{}).LaunchEnv()
	if !slices.Contains(env, OpencodeAPIKeyEnv+"=sk-vault-only") {
		t.Fatalf("LaunchEnv lacks the vault credential: %v", env)
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
		t.Errorf("hosted child env %v dir %q, want the hosted marker and WorkDir", cmd.Env, cmd.Dir)
	}
	t.Setenv(memory.UpdateChildEnv, "1")
	cmd, _, host, err = opencodeChild(context.Background(), "opencode", bin, []string{"A=1"}, argv, "hi", req)
	if err != nil || host != nil || slices.Contains(cmd.Env, hostedEnv+"=1") || !slices.Equal(cmd.Args[1:], argv) {
		t.Errorf("memory child: host %v err %v env %v args %v; want no host, no marker, argv as given", host, err, cmd.Env, cmd.Args)
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
	t.Setenv("COPILOT_HOME", t.TempDir())  // no real copilot config.json: CopilotStoredLogin reads nothing
	t.Setenv("GH_CONFIG_DIR", t.TempDir()) // no real gh hosts.yml: GHActiveAccount reads nothing
	for _, k := range []string{"ANTHROPIC_API_KEY", "GEMINI_API_KEY", "GH_TOKEN", "GITHUB_TOKEN", "COPILOT_GITHUB_TOKEN"} {
		t.Setenv(k, "")
	}
	return dataHome, cfgHome
}

// OpencodeCopilotModelIDs swaps the registry's "auto" haiku default for the provider's OpencodeFallback,
// while the sonnet alias's own (non-"auto") id passes through unchanged. The production registry's
// github-copilot entry no longer defaults haiku to "auto" (it now pins a concrete id), so this overlays
// a synthetic one to keep exercising the swap itself, independent of that literal.
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
	main, small, ok := OpencodeCopilotModelIDs(reg)
	if !ok {
		t.Fatal("OpencodeCopilotModelIDs: ok = false")
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
	_, small, ok := OpencodeCopilotModelIDs(reg)
	if !ok {
		t.Fatal("OpencodeCopilotModelIDs: ok = false")
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
