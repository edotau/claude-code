package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const overlay = `{
  "default": "gw",
  "fallback": ["anthropic"],
  "providers": {
    "gw": {"kind": "anthropic", "base_url": "https://gw.example.com",
      "routes": {"openai": "/v1", "responses": "https://other.example.com/r/"},
      "auth": {"type": "bearer", "env": "GW_KEY"}, "headers": {"X-Team": "a"},
      "models": {"opus": "claude-opus-5", "sonnet": "claude-sonnet-5", "haiku": "claude-haiku-4-5"}},
    "ollama": null
  }
}`

func load(t *testing.T) *Registry {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(PinProvider, "") // a session launched with a provider pin must not leak into the registry tests
	stubKeyring(t, nil)       // tests must never shell out to the real OS keyring
	reg, err := Parse(defaultsJSON, []byte(overlay))
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// stubKeyring replaces the keyringGet seam for the test's duration; a nil table always misses.
func stubKeyring(t *testing.T, table map[string]string) {
	t.Helper()
	prev := keyringGet
	keyringGet = func(name string) (string, bool) {
		v, ok := table[name]
		return v, ok
	}
	t.Cleanup(func() { keyringGet = prev })
}

func TestEmbeddedDefaultsValid(t *testing.T) {
	if _, err := Parse(defaultsJSON, nil); err != nil {
		t.Fatal(err)
	}
}

func TestOverlayAndRoutes(t *testing.T) {
	reg := load(t)
	if _, ok := reg.Providers["ollama"]; ok {
		t.Error("null overlay entry should remove the built-in")
	}
	gw, _ := reg.Get("gw")
	for d, want := range map[string]string{
		DialectAnthropic: "https://gw.example.com",
		DialectOpenAI:    "https://gw.example.com/v1",
		DialectResponses: "https://other.example.com/r",
	} {
		if got, ok := gw.Route(d); !ok || got != want {
			t.Errorf("Route(%s)=%q,%v want %q", d, got, ok, want)
		}
	}
	if _, ok := gw.Route(DialectGemini); ok {
		t.Error("gemini route should be absent")
	}
	if err := (&Registry{Default: "nope", Providers: map[string]*Provider{}}).Validate(); err == nil {
		t.Error("dangling default must fail validation")
	}
	keyedLogin := &Provider{Kind: DialectOpenAI, BaseURL: "https://x", Auth: Auth{Type: AuthBearer, Login: "codex"}}
	if err := (&Registry{Providers: map[string]*Provider{"x": keyedLogin}}).Validate(); err == nil {
		t.Error("auth.login without passthrough must fail validation")
	}
}

// opencode_fallback exists to substitute for a catalog "auto" id opencode can't resolve itself; letting
// opencode_fallback itself be "auto" would recreate the exact value it's supposed to replace.
func TestValidateRejectsOpencodeFallbackAuto(t *testing.T) {
	bad := &Provider{Kind: DialectAnthropic, BaseURL: "https://x", Auth: Auth{Type: AuthAPIKey, Env: "X_KEY"}, OpencodeFallback: "auto"}
	err := (&Registry{Providers: map[string]*Provider{"x": bad}}).Validate()
	if err == nil {
		t.Fatal("opencode_fallback: \"auto\" must fail validation")
	}
	if !strings.Contains(err.Error(), "opencode_fallback cannot itself be") {
		t.Errorf("error = %q, want it to name the opencode_fallback=auto rule", err)
	}
	good := &Provider{Kind: DialectAnthropic, BaseURL: "https://x", Auth: Auth{Type: AuthAPIKey, Env: "X_KEY"}, OpencodeFallback: "mai-code-1.1-flash"}
	if err := (&Registry{Providers: map[string]*Provider{"x": good}}).Validate(); err != nil {
		t.Errorf("a real opencode_fallback id must pass validation, got %v", err)
	}
}

func TestSelectPinsAndCrossProvider(t *testing.T) {
	reg := load(t)
	t.Setenv(SlotPin("haiku"), "openai:gpt-5-nano")
	if err := SetPins(map[string]string{PinModel: "claude-opus-4-8"}); err != nil {
		t.Fatal(err)
	}
	sel, err := Select(reg, "")
	if err != nil {
		t.Fatal(err)
	}
	if sel.Provider.Name != "gw" {
		t.Fatalf("provider %s", sel.Provider.Name)
	}
	if got := sel.ClientModel("opus"); got != "claude-opus-4-8" { // gw has one_m unset
		t.Errorf("opus client model %q", got)
	}
	if got := sel.ClientModel("haiku"); got != "openai:gpt-5-nano" {
		t.Errorf("haiku client model %q", got)
	}
	if !sel.NeedsRouter(reg) {
		t.Error("cross-provider slot must need the router")
	}
	if tgt := ParseTarget(reg, "qwen3:30b[1m]", sel.Provider); tgt.Provider != sel.Provider || tgt.Model != "qwen3:30b" {
		t.Errorf("unregistered prefix must stay a model: %+v", tgt)
	}
	b, _ := os.ReadFile(filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "env.d", "provider.env"))
	if len(b) == 0 {
		t.Error("pin file not written")
	}
}

// A passthrough (login) session never routes on the fallback ladder alone: the router's base URL disables Remote Control.
func TestNeedsRouterPassthroughFallback(t *testing.T) {
	reg := &Registry{Fallback: []string{"gw"}, Providers: map[string]*Provider{
		"sub": {Name: "sub", Kind: "anthropic", BaseURL: "https://api.anthropic.com", Auth: Auth{Type: AuthPassthrough}, Models: map[string]string{"opus": "o"}},
		"key": {Name: "key", Kind: "anthropic", BaseURL: "https://api.anthropic.com", Auth: Auth{Type: "x-api-key", Env: "K"}, Models: map[string]string{"opus": "o"}},
	}}
	for name, want := range map[string]bool{"sub": false, "key": true} {
		p := reg.Providers[name]
		sel := Selection{Provider: p, Slots: map[string]Target{"opus": {Provider: p, Model: "o"}}}
		if got := sel.NeedsRouter(reg); got != want {
			t.Errorf("%s: NeedsRouter = %v, want %v", name, got, want)
		}
	}
}

func TestFailoverTarget(t *testing.T) {
	reg := load(t)
	gw, _ := reg.Get("gw")
	or, _ := reg.Get("openrouter")
	oa, _ := reg.Get("openai")
	if tg, ok := FailoverTarget(gw, "claude-sonnet-5", or); !ok || tg.Model != "anthropic/claude-sonnet-5" {
		t.Errorf("slot mapping: %+v", tg)
	}
	if tg, ok := FailoverTarget(gw, "claude-haiku-9-9", oa); !ok || tg.Model != "gpt-5-nano" {
		t.Errorf("family mapping: %+v", tg)
	}
}

func TestCredentialLadderAndAuthorize(t *testing.T) {
	reg := load(t)
	gw, _ := reg.Get("gw")
	if CredentialPresent(gw) {
		t.Fatal("no credential expected yet")
	}
	os.MkdirAll(filepath.Dir(SecretsFile()), 0o700)
	os.WriteFile(SecretsFile(), []byte("export GW_KEY='from-file'\n"), 0o600)
	h := http.Header{"Authorization": {"Bearer client"}}
	if err := Authorize(context.Background(), gw, h); err != nil {
		t.Fatal(err)
	}
	if h.Get("Authorization") != "Bearer from-file" || h.Get("X-Team") != "a" {
		t.Errorf("headers %v", h)
	}
	t.Setenv("GW_KEY", "from-env")
	if v, _ := Credential(context.Background(), gw); v != "from-env" {
		t.Errorf("env must beat file, got %q", v)
	}
	gw.Auth = Auth{Type: AuthBearer, Command: "echo minted"}
	if v, _ := Credential(context.Background(), gw); v != "minted" {
		t.Errorf("command cred %q", v)
	}
	sub, _ := reg.Get("subscription")
	h = http.Header{"Authorization": {"Bearer oauth"}}
	if err := Authorize(context.Background(), sub, h); err != nil || h.Get("Authorization") != "Bearer oauth" {
		t.Errorf("passthrough must keep client auth: %v %v", err, h)
	}
}

// A Provider marshals its Headers redacted at the type; an inline gateway token never reaches a dump.
func TestProviderMarshalRedactsHeaders(t *testing.T) {
	tok := "sk" + "-abcdefghijklmnopqrstuvwxyz0123"
	p := Provider{Kind: DialectOpenAI, BaseURL: "https://gw", Headers: map[string]string{"X-Key": tok, "X-Plain": "v"}}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if strings.Contains(s, tok) || !strings.Contains(s, `"has_secret_headers":true`) || !strings.Contains(s, `"X-Plain":"v"`) {
		t.Errorf("marshal: %s", s)
	}
}

// PinDiff previews only real changes and never writes the pin file.
func TestPinDiffPreviewsWithoutWriting(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := SetPins(map[string]string{PinProvider: "gemini"}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(PinFile())
	got := PinDiff(map[string]string{PinProvider: "gemini", SlotPin("opus"): "x"})
	after, _ := os.ReadFile(PinFile())
	if len(got) != 1 || got[0] != SlotPin("opus")+": (unset) → x" || string(before) != string(after) {
		t.Fatalf("PinDiff = %q (file changed: %v)", got, string(before) != string(after))
	}
}

// StoredSecret must prefer a keyring hit over the file plane, and fall back to the file when the
// keyring misses; Credential must prefer the process env over both.
func TestStoredSecretAndCredentialOrder(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	os.MkdirAll(filepath.Dir(SecretsFile()), 0o700)
	os.WriteFile(SecretsFile(), []byte("export K='from-file'\n"), 0o600)

	stubKeyring(t, nil) // keyring misses -> file plane wins
	if got := StoredSecret("K"); got != "from-file" {
		t.Errorf("StoredSecret (keyring miss) = %q, want from-file", got)
	}

	stubKeyring(t, map[string]string{"K": "from-keyring"}) // keyring hit beats the file
	if got := StoredSecret("K"); got != "from-keyring" {
		t.Errorf("StoredSecret (keyring hit) = %q, want from-keyring", got)
	}

	p := &Provider{Auth: Auth{Type: AuthBearer, Env: "K"}}
	t.Setenv("K", "from-env")
	if v, _ := Credential(context.Background(), p); v != "from-env" {
		t.Errorf("Credential must prefer env over keyring/file, got %q", v)
	}
}

func TestHardQuotaAndDemote(t *testing.T) {
	for text, want := range map[string]bool{
		`{"error":{"code":429,"message":"You exceeded your current quota ... limit: 0, model: gemini-4-argon"}}`: true,
		`insufficient_quota`:                 true,
		`429 Too Many Requests, retry in 2s`: false,
		`rate limit exceeded`:                false,
	} {
		if got := HardQuota(text); got != want {
			t.Errorf("HardQuota(%q) = %v, want %v", text, got, want)
		}
	}
	p := &Provider{Name: "g", Models: map[string]string{"opus": "big", "sonnet": "mid", "haiku": "small", "fable": "other"}}
	if d, ok := Demote(p, "big"); !ok || d.Model != "mid" {
		t.Errorf("Demote(big) = %v %v, want mid", d.Model, ok)
	}
	if d, ok := Demote(p, "mid"); !ok || d.Model != "small" {
		t.Errorf("Demote(mid) = %v %v, want small", d.Model, ok)
	}
	if _, ok := Demote(p, "small"); ok {
		t.Error("Demote(small) must stop at the bottom of the ladder")
	}
	if _, ok := Demote(&Provider{Name: "one", Models: map[string]string{"opus": "x", "sonnet": "x"}}, "x"); ok {
		t.Error("Demote must not return the same model under another slot")
	}
}
