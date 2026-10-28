package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/outfmt"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
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
	return &providers.Provider{Name: "fake", Protocol: providers.ProtocolOpenAI, BaseURL: baseURL, Auth: providers.Auth{Type: providers.AuthNone}}
}

// cliScratch isolates CLAUDE_CONFIG_DIR, every credential env var, and PATH (no real copilot/gh/etc.
// binary reachable) so provider/model commands never touch real machine state.
func cliScratch(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", t.TempDir()) // no external credential helpers or other CLIs
	for _, k := range []string{"OPENAI_API_KEY", "GEMINI_API_KEY", "ANTHROPIC_API_KEY", "OPENROUTER_API_KEY", providers.EnvSessionProfile} {
		t.Setenv(k, "")
	}
}

func TestCmdModelsRejectsPositionalArguments(t *testing.T) {
	cases := []struct {
		name string
		args []string
		arg  string
	}{
		{"plain args", []string{"foo", "bar"}, "foo"},
		{"pin with trailing arg", []string{"--pin", "all=x", "foo"}, "foo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cliScratch(t)
			code, stdout, stderr := captureOutput(t, func() int { return cmdModels(tc.args) })
			if code != 2 {
				t.Fatalf("exit = %d, want 2; stdout=%q stderr=%q", code, stdout, stderr)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
			if want := "claude-code: models: unexpected argument \"" + tc.arg + "\"\n"; stderr != want {
				t.Errorf("stderr = %q, want %q", stderr, want)
			}
			if err := vault.View(func(tx *vault.Tx) error {
				if pins := tx.All(vault.Pins); len(pins) != 0 {
					t.Errorf("vault pins = %v, want no pin mutation", pins)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
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
	warn, err := validateCopilotPins("", []string{"opus=personal:claude-opus-5.5"})
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
	warn, err := validateCopilotPins("personal", []string{"opus=claude-opus-5.5"})
	if err != nil {
		t.Fatalf("validateCopilotPins: %v", err)
	}
	if !strings.Contains(warn, "could not verify") {
		t.Errorf("warn = %q, want the github-copilot catalog check to fire via --provider", warn)
	}
}

// listLive's copilot leg (dispatched via the provider name, listLiveCopilot no longer exists standalone)
// reports the copilot CLI as not found (never a panic) when it is absent from bin/.
func TestListLiveCopilotReportsMissingCLI(t *testing.T) {
	cliScratch(t)
	reg := &providers.Registry{}
	p := &providers.Provider{Name: "personal", Provider: "github"}
	code, _, stderr := captureOutput(t, func() int { return listLive(reg, p, outfmt.Text) })
	if code == 0 {
		t.Fatal("listLive: want a non-zero exit when the copilot CLI is missing")
	}
	if !strings.Contains(stderr, "copilot CLI not found") {
		t.Errorf("stderr = %q, want it to name the missing CLI", stderr)
	}
}

func TestCopilotModelIDsReportsMissingVendorBin(t *testing.T) {
	cliScratch(t)
	bin := pathstest.FakeVendor(t, "copilot", "exit 99")
	t.Setenv("PATH", filepath.Dir(bin))
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	ids, err := copilotModelIDs()
	if ids != nil {
		t.Errorf("ids = %v, want nil when bin/copilot is missing", ids)
	}
	want := "copilot CLI not found: copilot: not installed at " +
		filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "bin", "copilot") +
		" (claude-code install --tools=copilot)"
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
}

// listLive's non-copilot leg renders a fake provider's catalog as JSON rows, each carrying a use_case.
func TestListLiveJSONFormatsFakeCatalog(t *testing.T) {
	cliScratch(t)
	srv := fakeModelsServer(t, []string{"model-a", "model-b"})
	p := fakeProvider(srv.URL)
	reg := &providers.Registry{}
	code, stdout, _ := captureOutput(t, func() int { return listLive(reg, p, outfmt.JSON) })
	if code != 0 {
		t.Fatalf("listLive exit = %d", code)
	}
	var rows []struct {
		ID      string `json:"id"`
		UseCase string `json:"use_case"`
	}
	if err := json.Unmarshal([]byte(stdout), &rows); err != nil {
		t.Fatalf("stdout not a JSON array: %v\n%s", err, stdout)
	}
	if len(rows) != 2 || rows[0].ID != "model-a" || rows[1].ID != "model-b" {
		t.Errorf("rows = %+v", rows)
	}
	for _, r := range rows {
		if r.UseCase == "" {
			t.Errorf("row %+v: use_case must never be empty (models.UseCase always returns a label)", r)
		}
	}
}

// listLive's subscription (passthrough) leg falls back to the registry's configured models on a 401,
// and never prints the OAuth bearer token it tried — in the note, the table, or anywhere else.
func TestListLiveSubscriptionFallbackNeverPrintsToken(t *testing.T) {
	cliScratch(t)
	const secret = "sk-setup-token-super-secret-value"
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", secret)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	t.Cleanup(srv.Close)
	p := &providers.Provider{
		Name:     "sub",
		Protocol: providers.ProtocolAnthropic,
		BaseURL:  srv.URL,
		Auth:     providers.Auth{Type: providers.AuthCLI},
		Models:   map[string]string{"opus": "claude-opus-5-5"},
	}
	reg := &providers.Registry{}
	code, stdout, stderr := captureOutput(t, func() int { return listLive(reg, p, outfmt.Text) })
	if code != 0 {
		t.Fatalf("listLive exit = %d, stderr=%s", code, stderr)
	}
	if strings.Contains(stdout, secret) || strings.Contains(stderr, secret) {
		t.Fatalf("output leaked the OAuth token\nstdout=%q\nstderr=%q", stdout, stderr)
	}
	if !strings.Contains(stderr, "live catalog needs an API key") {
		t.Errorf("stderr = %q, want the fallback note", stderr)
	}
	if !strings.Contains(stdout, "claude-opus-5-5") {
		t.Errorf("stdout = %q, want the registry's configured model in the fallback table", stdout)
	}
}

// printModelTable aligns every column to the widest cell in it, including the header, and drops a
// gemini "models/" id prefix for display only.
func TestPrintModelTableAligns(t *testing.T) {
	infos := []providers.ModelInfo{
		{ID: "models/gemini-2.5-flash", Name: "Gemini 2.5 Flash", UseCase: "Balanced", Capabilities: ""},
		{ID: "x", Name: "y", UseCase: "High volume", Capabilities: "vision · x2"},
	}
	_, stdout, _ := captureOutput(t, func() int { printModelTable(infos); return 0 })
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want a header + 2 rows:\n%s", len(lines), stdout)
	}
	if strings.Contains(lines[1], "models/") {
		t.Errorf("row kept the gemini models/ prefix: %q", lines[1])
	}
	// Every line's "Use case" column must start at the same byte offset.
	idx := strings.Index(lines[0], "Use case")
	for i, l := range lines {
		if got := strings.Index(l, strings.Fields(l)[0]); got != 0 {
			t.Fatalf("line %d not left-aligned: %q", i, l)
		}
	}
	for i := 1; i < len(lines); i++ {
		col := strings.Index(lines[i], infos[i-1].UseCase)
		if col != idx {
			t.Errorf("line %q: Use case column at %d, want %d (header's)", lines[i], col, idx)
		}
	}
}

// applyModelPins qualifies a bare model spec with --provider when it differs from the session's
// subscribed provider, so the pin resolves against the provider it was validated for.
func TestApplyModelPinsQualifiesAcrossSessionProvider(t *testing.T) {
	cliScratch(t)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := providers.SetPins(map[string]string{providers.PinProfile: "anthropic"}); err != nil {
		t.Fatal(err)
	}
	if code, handled := applyModelPins(reg, "personal", false, false, []string{"opus=claude-opus-5.5"}); handled || code != 0 {
		t.Fatalf("applyModelPins: code=%d handled=%v, want 0, false", code, handled)
	}
	if got := providers.Pin(providers.SlotPin("opus")); got != "personal:claude-opus-5.5" {
		t.Errorf("CLAUDE_OPUS_MODEL = %q, want %q", got, "personal:claude-opus-5.5")
	}
}

// --provider equal to the session's own subscribed provider stores the bare spec, unqualified.
func TestApplyModelPinsBareWhenProviderMatchesSession(t *testing.T) {
	cliScratch(t)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := providers.SetPins(map[string]string{providers.PinProfile: "personal"}); err != nil {
		t.Fatal(err)
	}
	if code, handled := applyModelPins(reg, "personal", false, false, []string{"opus=claude-opus-5.5"}); handled || code != 0 {
		t.Fatalf("applyModelPins: code=%d handled=%v, want 0, false", code, handled)
	}
	if got := providers.Pin(providers.SlotPin("opus")); got != "claude-opus-5.5" {
		t.Errorf("CLAUDE_OPUS_MODEL = %q, want the bare spec %q", got, "claude-opus-5.5")
	}
}

// qualifyPinSpec passes an already-qualified "provider:model" spec through unchanged, even when
// --provider names a different provider than the one in the spec.
func TestQualifyPinSpecPassesThroughAlreadyQualified(t *testing.T) {
	cliScratch(t)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	got := qualifyPinSpec(reg, "personal", "anthropic:claude-opus-5.5")
	if got != "anthropic:claude-opus-5.5" {
		t.Errorf("qualifyPinSpec = %q, want the already-qualified spec unchanged", got)
	}
}

// --pin all=... qualifies CLAUDE_MODEL the same way a per-slot pin does.
func TestApplyModelPinsAllSlotQualifies(t *testing.T) {
	cliScratch(t)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := providers.SetPins(map[string]string{providers.PinProfile: "anthropic"}); err != nil {
		t.Fatal(err)
	}
	if code, handled := applyModelPins(reg, "personal", false, false, []string{"all=claude-opus-5.5"}); handled || code != 0 {
		t.Fatalf("applyModelPins: code=%d handled=%v, want 0, false", code, handled)
	}
	if got := providers.Pin(providers.PinModel); got != "personal:claude-opus-5.5" {
		t.Errorf("CLAUDE_MODEL = %q, want %q", got, "personal:claude-opus-5.5")
	}
}

// doctor's/profile's output over a github-copilot pin or route never prints anything secret-looking:
// no bearer token, no raw API key value, only the env var NAME.
func TestValidateCopilotPinsOutputNamesNoSecret(t *testing.T) {
	cliScratch(t)
	t.Setenv("GH_TOKEN", "super-secret-token-value")
	warn, _ := validateCopilotPins("", []string{"opus=personal:claude-opus-5.5"})
	if strings.Contains(warn, "super-secret-token-value") {
		t.Errorf("warn leaked a credential: %q", warn)
	}
}
