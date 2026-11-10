package codex

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

const (
	testRouterBase   = "http://127.0.0.1:9999"
	testRouterSecret = "rtr-secret-xyz"
)

// stubProxy stubs the router (ensure, or a fixed base when nil), its secret and session mints for the test.
func stubProxy(t *testing.T, ensure func(context.Context) (string, error)) {
	t.Helper()
	oe, os_, om := agent.EnsureRouter, agent.RouterSecret, agent.MintSession
	t.Cleanup(func() { agent.EnsureRouter, agent.RouterSecret, agent.MintSession = oe, os_, om })
	agent.StubProxyForTest(testRouterBase, testRouterSecret)
	if ensure != nil {
		agent.EnsureRouter = ensure
	}
}

// loadTarget is provider's built-in row with every provider key set, under a fresh config dir.
func loadTarget(t *testing.T, provider, model string) providers.Target {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	for _, k := range []string{"OPENAI_API_KEY", "GEMINI_API_KEY", "GATEWAY_API_KEY", "ANTHROPIC_API_KEY"} {
		t.Setenv(k, "sk-SECRET-123")
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	p, err := reg.Get(provider)
	if err != nil {
		t.Fatal(err)
	}
	return providers.Target{Provider: p, Model: model}
}

// The codex status line may arrive on stderr; only its first line, cut at " - ", becomes the detail.
func TestCodexDetailFromCombinedOutput(t *testing.T) {
	vaulttest.UseKeyring(t)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	pathstest.FakeVendor(t, "codex", "#!/bin/sh\necho 'Logged in using ChatGPT - me@example.com' >&2\necho second line\n")
	p := &providers.Provider{Name: "x", Protocol: providers.ProtocolAnthropic, Auth: providers.Auth{Type: providers.AuthCLI, Login: "codex"}}
	v, d := agent.SigninFor(p).Check(context.Background())
	if v != providers.VerdictOK || d != "Logged in using ChatGPT" {
		t.Errorf("stderr-only status = %s %q", v, d)
	}
}

func TestAuthNoneNotRouted(t *testing.T) {
	stubProxy(t, func(context.Context) (string, error) {
		t.Error("router ensured for AuthNone")
		return "", errors.New("x")
	})
	tg := loadTarget(t, "openai", "gpt-5")
	p := *tg.Provider
	p.Auth = providers.Auth{Type: providers.AuthNone}
	tg.Provider = &p
	env, argv, err := agent.SchemaFor("codex").Configure(context.Background(), tg, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(argv, " "), testRouterBase) || strings.Contains(strings.Join(env, " "), testRouterSecret) {
		t.Errorf("AuthNone was routed: %q %q", env, argv)
	}
}

// codex gets -c instructions / developer_instructions and a strict --output-schema file that is gone after the run.
func TestCodexRunFlagsAndSchemaFileCleanup(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	stubProxy(t, nil)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	vaulttest.Put(t, vault.Overlay, "providers", fmt.Sprintf(`{"providers":{"openai":{"toai":{"protocol":"responses","base_url":%q,"auth":{"type":"none"},"models":{"opus":"gpt-x"}}}}}`, srv.URL+"/v1"))
	dir := t.TempDir()
	argvFile, capture := filepath.Join(dir, "codex.argv"), filepath.Join(dir, "schema.cap")
	// help prints no --json: codexHasJSON is cached per process and must not flip for other tests
	body := `cat >/dev/null
if [ "$1" = exec ] && [ "$2" = --help ]; then exit 0; fi
printf '%s\n' "$@" > '` + argvFile + `'
prev=
for a in "$@"; do
  if [ "$prev" = --output-schema ]; then cp "$a" '` + capture + `'; printf '%s\n' "$a" > '` + capture + `.path'; ls -l "$a" | cut -c1-10 > '` + capture + `.mode'; fi
  prev=$a
done
echo '{"type":"item.completed","item":{"id":"i","type":"agent_message","text":"ok"}}'`
	pathstest.FakeVendor(t, "codex", body)
	e, err := agent.Lookup("codex")
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.Run(context.Background(), agent.RunOptions{Prompt: "task", Provider: "toai", Tools: true,
		SystemPrompt: `be "terse"`, AppendSystemPrompt: "APP", OutputSchema: `{"type":"object","properties":{"a":{"type":"string"}}}`}, wire.Discard)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if !followedBy(args, "-c", `instructions="be \"terse\""`) || !followedBy(args, "-c", `developer_instructions="APP"`) {
		t.Errorf("argv %q lacks the -c instructions overrides", args)
	}
	if args[0] != "exec" || !slices.Contains(args, "--output-schema") {
		t.Errorf("argv %q: want exec first and --output-schema", args)
	}
	assertCodexSchemaCleanup(t, capture)
}

// followedBy reports whether args holds flag immediately followed by val.
func followedBy(args []string, flag, val string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == val {
			return true
		}
	}
	return false
}

func assertCodexSchemaCleanup(t *testing.T, capture string) {
	t.Helper()
	schema, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(schema), `"additionalProperties":false`) || !strings.Contains(string(schema), `"required":["a"]`) {
		t.Errorf("schema handed to codex = %s", schema)
	}
	if mode, _ := os.ReadFile(capture + ".mode"); strings.TrimSpace(string(mode)) != "-rw-------" {
		t.Errorf("schema file mode = %q, want -rw-------", mode)
	}
	path, _ := os.ReadFile(capture + ".path")
	if _, err := os.Stat(strings.TrimSpace(string(path))); !os.IsNotExist(err) {
		t.Errorf("schema temp file %s survived the run (err=%v)", path, err)
	}
}

func fallbackTargetFixture(t *testing.T) (string, *providers.Registry, providers.Selection) {
	t.Helper()
	t.Setenv("PATH", t.TempDir()) // no real codex: login detection falls back to the auth.json fixture
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("CODEX_HOME", filepath.Join(dir, "codex"))
	t.Setenv("GH_TOKEN", "")
	for _, k := range []string{"OPENAI_API_KEY", "GEMINI_API_KEY", "GATEWAY_API_KEY", "ANTHROPIC_API_KEY"} {
		t.Setenv(k, "")
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	sub, err := reg.Get("ultra")
	if err != nil {
		t.Fatal(err)
	}
	sel := providers.Selection{Provider: sub, Aliases: map[string]providers.Target{"opus": {Provider: sub, Model: "claude-opus-5-5"}}}
	return dir, reg, sel
}

func TestResolveTargetFallsBackWhenSessionProviderCannotServe(t *testing.T) {
	dir, reg, sel := fallbackTargetFixture(t)
	sub := sel.Provider
	// codex: subscription can't serve it, no ChatGPT login present -> target unchanged, no note.
	target, note, err := agent.SchemaFor("codex").ResolveTarget(reg, sel, false)
	if err != nil {
		t.Fatal(err)
	}
	if note != "" {
		t.Fatalf("expected no fallback without a codex login, got %q", note)
	}
	if target.Provider != sub {
		t.Fatalf("expected unchanged target, got %s", target.Provider.Name)
	}

	// With a ChatGPT login present, codex falls back to the chatgpt provider.
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, "codex", "auth.json")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "codex", "auth.json"), []byte(`{"tokens":{"access_token":"x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	target, note, err = agent.SchemaFor("codex").ResolveTarget(reg, sel, false)
	if err != nil {
		t.Fatal(err)
	}
	if target.Provider == nil || target.Provider.Name != "chatgpt" {
		t.Fatalf("expected fallback to chatgpt, got %v", target.Provider)
	}
	if !strings.Contains(note, "chatgpt") {
		t.Fatalf("expected a note naming chatgpt, got %q", note)
	}

	assertFallbackModelPinAndExplicitProvider(t, reg, sel)
}

func assertFallbackModelPinAndExplicitProvider(t *testing.T, reg *providers.Registry, sel providers.Selection) {
	t.Helper()
	// The CODEX_MODEL pin beats chatgpt's opus alias and codexOwnModel's config.toml (unset here).
	vaulttest.SetRecord(t, "codex", "model", "")
	vaulttest.SetRecord(t, "codex", "model", "gpt-6.1-sol")
	target, _, err := agent.SchemaFor("codex").ResolveTarget(reg, sel, false)
	if err != nil {
		t.Fatal(err)
	}
	if target.Model != "gpt-6.1-sol" {
		t.Fatalf("CODEX_MODEL pin = gpt-6.1-sol, want it to win the fallback target's model, got %q", target.Model)
	}

	// An explicit provider choice is never overridden by a fallback.
	target, note, err := agent.SchemaFor("codex").ResolveTarget(reg, sel, true)
	if err != nil {
		t.Fatal(err)
	}
	if note != "" || target.Provider != sel.Provider {
		t.Fatalf("explicit provider must not fall back, got target=%v note=%q", target.Provider, note)
	}
}

// Interactive codex (nil Headless) carries the CODEX_EFFORT pin; headless Effort still wins.
func TestCodexInteractiveCarriesEffortPin(t *testing.T) {
	stubProxy(t, nil)
	tg := loadTarget(t, "openai", "gpt-5")
	vaulttest.SetRecord(t, "codex", "effort", "medium")
	_, argv, err := agent.SchemaFor("codex").Configure(context.Background(), tg, "")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(argv, `model_reasoning_effort="medium"`) {
		t.Errorf("interactive argv = %q, want the effort pin", argv)
	}
	_, argv, err = agent.SchemaFor("codex").ConfigureHeadless(context.Background(), tg, agent.Headless{Prompt: "hi", Effort: "low"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(argv, `model_reasoning_effort="low"`) || slices.Contains(argv, `model_reasoning_effort="medium"`) {
		t.Errorf("headless argv = %q, want explicit low over the pin", argv)
	}
}

// A credential-shaped header value must reach codex through env_http_headers + env, never argv.
func TestCodexCredentialHeaderTravelsInEnvNotArgv(t *testing.T) {
	secret := "sk-" + strings.Repeat("a1B2", 10)
	p := &providers.Provider{Name: "acme", Headers: map[string]string{
		"Authorization": "Bearer " + secret,
		"X-Team":        "platform",
	}}
	env, argv, err := codex(context.Background(), p, "https://example.test/v1", "gpt-x", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, "\n")
	if strings.Contains(joined, secret) {
		t.Fatalf("argv leaks the secret: %s", joined)
	}
	for _, want := range []string{
		`env_http_headers = { "Authorization" = "CLAUDE_CODE_CODEX_HDR_AUTHORIZATION" }`,
		`http_headers = { "X-Team" = "platform" }`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv missing %q in %s", want, joined)
		}
	}
	wantEnv := "CLAUDE_CODE_CODEX_HDR_AUTHORIZATION=Bearer " + secret
	var found bool
	for _, e := range env {
		found = found || e == wantEnv
	}
	if !found {
		t.Errorf("env lacks %q: %q", "CLAUDE_CODE_CODEX_HDR_AUTHORIZATION", env)
	}
}

const tokensJSON = `{"tokens":{"id_token":"x"}}`

func resetAuthMemos(t *testing.T) {
	t.Helper()
	clear := func() {
		authStateMu.Lock()
		authStateMemo, authSoftMemo, authSoftUntil = nil, nil, time.Time{}
		authStateMu.Unlock()
	}
	clear()
	t.Cleanup(clear)
}

// A keyring-held ChatGPT login reads as present on every call, with or without auth.json to stat.
func TestAuthStatusKeyringLoginStaysPresent(t *testing.T) {
	forEachKeyringOS(t, func(t *testing.T) {
		t.Setenv("CODEX_HOME", t.TempDir())
		resetAuthMemos(t)
		orig := codexKeychainCmd
		codexKeychainCmd = func(context.Context, ...string) ([]byte, error) { return []byte(tokensJSON), nil }
		t.Cleanup(func() { codexKeychainCmd = orig })
		for i := range 2 {
			if st := (codexEngine{}).AuthStatus(); !st.Present || st.Err != nil {
				t.Fatalf("call %d: keyring login not present: %+v", i, st)
			}
		}
	})
}

// With auth.json present the mtime memo answers without touching the keyring, and a logout is seen at once.
func TestAuthStatusKeyedMemoSkipsKeyring(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	resetAuthMemos(t)
	orig := codexKeychainCmd
	codexKeychainCmd = func(context.Context, ...string) ([]byte, error) {
		t.Error("keyring consulted although auth.json holds the login")
		return nil, nil
	}
	t.Cleanup(func() { codexKeychainCmd = orig })
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(tokensJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		if st := (codexEngine{}).AuthStatus(); !st.Present {
			t.Fatalf("call %d: auth.json login not present: %+v", i, st)
		}
	}
}

// forEachKeyringOS runs body under each OS with a keyring plane; an unsupported OS never consults the keyring.
func forEachKeyringOS(t *testing.T, body func(t *testing.T)) {
	t.Helper()
	for _, goos := range []string{"linux", "darwin", "windows"} {
		t.Run(goos, func(t *testing.T) {
			orig := codexGOOS
			codexGOOS = goos
			t.Cleanup(func() { codexGOOS = orig })
			if goos != "windows" {
				body(t)
				return
			}
			resetAuthMemos(t)
			t.Setenv("CODEX_HOME", t.TempDir())
			stubKeyringLogin(t, func() bool { t.Error("keyring consulted on an OS without a keyring plane"); return true })
			if st := (codexEngine{}).AuthStatus(); st.Present {
				t.Fatalf("keyring login present on %s: %+v", goos, st)
			}
		})
	}
}

// stubKeyringLogin swaps the keyring and CLI probes: signedIn() decides what the keyring holds.
func stubKeyringLogin(t *testing.T, signedIn func() bool) {
	t.Helper()
	origKC, origSt := codexKeychainCmd, codexStatus
	codexKeychainCmd = func(context.Context, ...string) ([]byte, error) {
		if signedIn() {
			return []byte(tokensJSON), nil
		}
		return nil, nil
	}
	codexStatus = func(context.Context, string) ([]byte, error) { return []byte("Logged in using an API key"), nil }
	t.Cleanup(func() { codexKeychainCmd, codexStatus = origKC, origSt })
}

// A cached keyring login for one CODEX_HOME is not served for another home.
func TestAuthStatusSoftMemoKeyedOnHome(t *testing.T) {
	forEachKeyringOS(t, func(t *testing.T) {
		resetAuthMemos(t)
		signedIn := true
		stubKeyringLogin(t, func() bool { return signedIn })
		t.Setenv("CODEX_HOME", t.TempDir())
		if st := (codexEngine{}).AuthStatus(); !st.Present {
			t.Fatalf("first home: %+v", st)
		}
		signedIn = false
		t.Setenv("CODEX_HOME", t.TempDir())
		if st := (codexEngine{}).AuthStatus(); st.Present {
			t.Fatalf("second home served the first home's cached login: %+v", st)
		}
	})
}

// A signed-out result is never cached, so a login from another process shows on the next call.
func TestAuthStatusSignedOutNotCached(t *testing.T) {
	forEachKeyringOS(t, func(t *testing.T) {
		resetAuthMemos(t)
		signedIn := false
		stubKeyringLogin(t, func() bool { return signedIn })
		t.Setenv("CODEX_HOME", t.TempDir())
		if st := (codexEngine{}).AuthStatus(); st.Present {
			t.Fatalf("signed out reads present: %+v", st)
		}
		signedIn = true
		if st := (codexEngine{}).AuthStatus(); !st.Present {
			t.Fatalf("login from another process not seen: %+v", st)
		}
	})
}

// Verify forgets the soft memo so a fresh probe follows it.
func TestVerifyDropsAuthMemo(t *testing.T) {
	forEachKeyringOS(t, func(t *testing.T) {
		resetAuthMemos(t)
		signedIn := true
		stubKeyringLogin(t, func() bool { return signedIn })
		t.Setenv("CODEX_HOME", t.TempDir())
		if st := (codexEngine{}).AuthStatus(); !st.Present {
			t.Fatalf("seed: %+v", st)
		}
		signedIn = false
		_, _ = (codexEngine{}).Verify(context.Background())
		if st := (codexEngine{}).AuthStatus(); st.Present {
			t.Fatalf("memo survived Verify: %+v", st)
		}
	})
}
