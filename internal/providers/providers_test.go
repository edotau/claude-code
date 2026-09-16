package providers

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
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
	reg, err := Parse(defaultsJSON, []byte(overlay))
	if err != nil {
		t.Fatal(err)
	}
	return reg
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
