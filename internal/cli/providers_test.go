package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/outfmt"
	"github.com/edotau/claude-code/internal/providers"
)

// fakeModelsServer serves the OpenAI-shaped {"data":[{"id":...}]} catalog listLive's non-copilot leg GETs.
func fakeModelsServer(t *testing.T, ids []string) *httptest.Server {
	t.Helper()
	type entry struct {
		ID string `json:"id"`
	}
	data := make([]entry, len(ids))
	for i, id := range ids {
		data[i] = entry{ID: id}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(struct {
			Data []entry `json:"data"`
		}{data})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fakeProvider is a minimal, unauthenticated openai-dialect provider pointed at a local httptest server.
func fakeProvider(baseURL string) *providers.Provider {
	return &providers.Provider{Name: "fake", Kind: providers.DialectOpenAI, BaseURL: baseURL, Auth: providers.Auth{Type: providers.AuthNone}}
}

// cliScratch isolates CLAUDE_CONFIG_DIR, every credential env var, and PATH (no real copilot/gh/etc.
// binary reachable) so provider/model commands never touch real machine state.
func cliScratch(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", t.TempDir()) // no copilot CLI on PATH: exercises the "not found"/"unreachable" legs
	for _, k := range []string{"OPENAI_API_KEY", "GEMINI_API_KEY", "ANTHROPIC_API_KEY", "OPENROUTER_API_KEY", "HARNESS_PROVIDER", "HARNESS_MODEL", "HARNESS_OPUS_MODEL", "HARNESS_SONNET_MODEL", "HARNESS_HAIKU_MODEL", "HARNESS_FABLE_MODEL"} {
		t.Setenv(k, "")
	}
}

// A pin naming a provider other than github-copilot is never checked against the copilot catalog.
func TestValidateCopilotPinsSkipsOtherProviders(t *testing.T) {
	cliScratch(t)
	warn, err := validateCopilotPins("", []string{"opus=openai:gpt-5"})
	if err != nil || warn != "" {
		t.Fatalf("warn=%q err=%v, want no-op for a non-copilot pin", warn, err)
	}
}

// A github-copilot pin warns (never fails) when the live catalog itself cannot be reached: the pin
// is still written by the caller, not blocked, since this is an availability signal, not validation.
func TestValidateCopilotPinsWarnsWhenCatalogUnreachable(t *testing.T) {
	cliScratch(t)
	warn, err := validateCopilotPins("", []string{"opus=github-copilot:claude-opus-5.5"})
	if err != nil {
		t.Fatalf("validateCopilotPins: %v, want a warning not an error", err)
	}
	if !strings.Contains(warn, "could not verify") {
		t.Errorf("warn = %q, want it to name the unreachable catalog", warn)
	}
}

// The provider prefix form (slot=provider:model) and the --provider flag fallback form (slot=model)
// both key off githubCopilotProvider the same way.
func TestValidateCopilotPinsProviderFlagFallback(t *testing.T) {
	cliScratch(t)
	warn, err := validateCopilotPins("github-copilot", []string{"opus=claude-opus-5.5"})
	if err != nil {
		t.Fatalf("validateCopilotPins: %v", err)
	}
	if !strings.Contains(warn, "could not verify") {
		t.Errorf("warn = %q, want the github-copilot catalog check to fire via --provider", warn)
	}
}

// listLiveCopilot reports the copilot CLI as not found (never a panic) when it is absent from PATH.
func TestListLiveCopilotReportsMissingCLI(t *testing.T) {
	cliScratch(t)
	code, _, stderr := captureOutput(t, func() int { return listLiveCopilot(outfmt.Text) })
	if code == 0 {
		t.Fatal("listLiveCopilot: want a non-zero exit when the copilot CLI is missing")
	}
	if !strings.Contains(stderr, "copilot CLI not found") {
		t.Errorf("stderr = %q, want it to name the missing CLI", stderr)
	}
}

// listLive's non-copilot leg renders a fake provider's catalog as JSON rows, id-only.
func TestListLiveJSONFormatsFakeCatalog(t *testing.T) {
	cliScratch(t)
	srv := fakeModelsServer(t, []string{"model-a", "model-b"})
	p := fakeProvider(srv.URL)
	code, stdout, _ := captureOutput(t, func() int { return listLive(p, outfmt.JSON) })
	if code != 0 {
		t.Fatalf("listLive exit = %d", code)
	}
	var rows []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(stdout), &rows); err != nil {
		t.Fatalf("stdout not a JSON array: %v\n%s", err, stdout)
	}
	if len(rows) != 2 || rows[0].ID != "model-a" || rows[1].ID != "model-b" {
		t.Errorf("rows = %+v", rows)
	}
}

// doctor's/profile's output over a github-copilot pin or route never prints anything secret-looking:
// no bearer token, no raw API key value, only the env var NAME.
func TestValidateCopilotPinsOutputNamesNoSecret(t *testing.T) {
	cliScratch(t)
	t.Setenv("GH_TOKEN", "super-secret-token-value")
	warn, _ := validateCopilotPins("", []string{"opus=github-copilot:claude-opus-5.5"})
	if strings.Contains(warn, "super-secret-token-value") {
		t.Errorf("warn leaked a credential: %q", warn)
	}
}
