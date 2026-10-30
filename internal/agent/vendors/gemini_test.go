package vendors

import (
	"context"
	"github.com/edotau/claude-code/internal/providers"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// agy has no custom base URL, so a gateway provider for the gemini vendor must fail fast with a clear error.
func TestGeminiGateway(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GW_KEY", secret)
	p := &providers.Provider{Name: "gw", Protocol: providers.ProtocolGemini, BaseURL: "https://gw.example/gemini",
		Auth: providers.Auth{Type: providers.AuthKey, Env: "GW_KEY"}, Headers: map[string]string{"X-Team": "a"}}
	_, _, err := Configure(context.Background(), "gemini", providers.Target{Provider: p, Model: "m"}, "")
	if err == nil || !strings.Contains(err.Error(), "no custom base URL") {
		t.Fatalf("err = %v, want a clear \"no custom base URL\" error", err)
	}
}

// gemini runs only on agy's cached Google login (AgyLoginMarker under HOME): no marker refuses to
// configure (no GEMINI_API_KEY fallback), a marker on disk lets it through with no key injected.
func TestGeminiLoginPrecedence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if GeminiLoginPresent() {
		t.Fatal("no marker on disk must read as not logged in")
	}
	tg := target(t, "antigravity", "gemini-2.5-pro")
	// target() reset HOME to its own tempdir; point it back at ours for the marker check.
	t.Setenv("HOME", home)

	if _, _, err := ConfigureHeadless(context.Background(), "gemini", tg, Headless{Prompt: "hi"}); err == nil || !strings.Contains(err.Error(), "not signed in") {
		t.Fatalf("without a login marker, err = %v, want a not-signed-in error", err)
	}

	marker := AgyLoginMarker(home)
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !GeminiLoginPresent() {
		t.Fatal("marker on disk must read as logged in")
	}

	env, _, err := ConfigureHeadless(context.Background(), "gemini", tg, Headless{Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := envMap(env)["GEMINI_API_KEY"]; ok {
		t.Errorf("with a login marker, GEMINI_API_KEY must not be set, got %q", got)
	}
}
