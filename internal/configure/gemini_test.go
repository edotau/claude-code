package configure

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secrets"
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
