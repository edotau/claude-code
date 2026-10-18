package configure

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/vendors"
)

// fakeAnthropicProvider is a provider whose catalog route is a local server: ProbeCredential GETs it
// with the candidate in X-Api-Key, so "verified" vs "rejected" is driven without ever touching a real API.
func fakeAnthropicProvider(t *testing.T, want string) *providers.Provider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != want {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[]}`))
	}))
	t.Cleanup(srv.Close)
	return &providers.Provider{
		Name: "gemini", Kind: providers.DialectAnthropic, BaseURL: srv.URL,
		Auth: providers.Auth{Type: providers.AuthAPIKey, Env: "GEMINI_API_KEY"},
	}
}

func TestPresence(t *testing.T) {
	if presence(true) != "present" || presence(false) != "absent" {
		t.Errorf("presence(true/false) = %q/%q", presence(true), presence(false))
	}
}

func TestPlaneLabel(t *testing.T) {
	if planeLabel("file") != "secrets.env" {
		t.Errorf("planeLabel(file) = %q", planeLabel("file"))
	}
	if planeLabel("keyring") != "keyring" {
		t.Errorf("planeLabel(keyring) = %q", planeLabel("keyring"))
	}
}

// No key at all: status is the plain "no key" line, no probe attempted.
func TestGeminiKeyStatusNoKey(t *testing.T) {
	if got := geminiKeyStatus(false, nil); got != "no key" {
		t.Errorf("geminiKeyStatus(false, nil) = %q, want %q", got, "no key")
	}
}

// A present key that the live probe accepts reports its plane and "probe ok".
func TestGeminiKeyStatusPresentProbeOK(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("PATH", t.TempDir()) // no keyring tool: forces the file plane
	t.Setenv("GEMINI_API_KEY", "good-value-123")
	if _, err := secrets.Store("GEMINI_API_KEY", "good-value-123", true); err != nil {
		t.Fatal(err)
	}
	p := fakeAnthropicProvider(t, "good-value-123")
	got := geminiKeyStatus(true, p)
	if !strings.Contains(got, "stored in secrets.env") || !strings.Contains(got, "probe ok") {
		t.Errorf("geminiKeyStatus = %q, want secrets.env + probe ok", got)
	}
}

// A present key the live probe rejects reports "probe failed", not "probe ok".
func TestGeminiKeyStatusPresentProbeRejected(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("GEMINI_API_KEY", "bad-value")
	p := fakeAnthropicProvider(t, "the-only-accepted-value")
	got := geminiKeyStatus(true, p)
	if !strings.Contains(got, "probe failed") {
		t.Errorf("geminiKeyStatus = %q, want probe failed", got)
	}
}

// pickGeminiAuth's Login row status is vendors.GeminiLoginPresent(): a selected Google login reports
// "present", everything else (no settings.json, or a key auth selected) reports "absent". This is the
// testable half of the row — pickGeminiAuth itself calls tui.PickBack directly with no injectable seam
// (same limitation noted for pickClaudeBackend), so the Login-row-first ordering can only be read from
// internal/configure/gemini.go, not driven here.
func TestGeminiLoginRowStatusReflectsLoginPresence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GEMINI_CLI_HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".gemini"), 0o755); err != nil {
		t.Fatal(err)
	}

	if got := presence(vendors.GeminiLoginPresent()); got != "absent" {
		t.Errorf("no settings.json: presence = %q, want absent", got)
	}

	if err := os.WriteFile(filepath.Join(home, ".gemini", "settings.json"),
		[]byte(`{"security":{"auth":{"selectedType":"gemini-api-key"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := presence(vendors.GeminiLoginPresent()); got != "absent" {
		t.Errorf("gemini-api-key selected: presence = %q, want absent", got)
	}

	if err := os.WriteFile(filepath.Join(home, ".gemini", "settings.json"),
		[]byte(`{"security":{"auth":{"selectedType":"oauth-personal"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := presence(vendors.GeminiLoginPresent()); got != "present" {
		t.Errorf("oauth-personal selected: presence = %q, want present", got)
	}
}
