package configure

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/copilotsdk"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
)

// fakeCopilotBin makes paths.VendorBin("copilot") resolve, so copilotCatalog gets past that check and
// reaches the stubbed copilotListModelsFn seam instead of erroring out before ever calling it. It never
// runs: every test here replaces copilotListModelsFn, so nothing execs the real CLI or touches gh/GitHub.
// Must run after menuTestEnv (it writes under paths.BinDir(), which menuTestEnv's HOME already pins) so
// the vault/overlay a test's SetPins/Pin calls hit stays the one menuTestEnv set up — never a second,
// freshly created config dir.
func fakeCopilotBin(t *testing.T) {
	t.Helper()
	bin := paths.BinDir()
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "copilot"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// stubCatalog replaces copilotListModelsFn with a fixed result and restores it on cleanup.
func stubCatalog(t *testing.T, models []copilotsdk.Model, err error) *int {
	t.Helper()
	orig := copilotListModelsFn
	calls := 0
	copilotListModelsFn = func(ctx context.Context, o copilotsdk.Options) ([]copilotsdk.Model, error) {
		calls++
		return models, err
	}
	t.Cleanup(func() { copilotListModelsFn = orig })
	return &calls
}

// copilotCatalog fetches the SDK catalog once per dialog: a second call (from a different row) must hit
// the cache, not re-invoke the seam (re-spawning the copilot runtime on every row would be a real cost).
func TestCopilotCatalogFetchedOncePerDialog(t *testing.T) {
	menuTestEnv(t)
	fakeCopilotBin(t)
	calls := stubCatalog(t, []copilotsdk.Model{{ID: "claude-sonnet-5", Enabled: true}}, nil)

	d := &dialog{out: discardWriter{}}
	if _, err := copilotCatalog(d); err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if _, err := copilotCatalog(d); err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if _, ok := copilotCatalogModel(d, "claude-sonnet-5"); !ok {
		t.Fatal("copilotCatalogModel: want the cached model found")
	}
	if *calls != 1 {
		t.Errorf("copilotListModelsFn called %d times, want 1 (cached per dialog)", *calls)
	}
}

// A fetch error is cached too: a second call must not retry the seam.
func TestCopilotCatalogCachesError(t *testing.T) {
	menuTestEnv(t)
	fakeCopilotBin(t)
	wantErr := errors.New("rpc: not authenticated")
	calls := stubCatalog(t, nil, wantErr)

	d := &dialog{out: discardWriter{}}
	if _, err := copilotCatalog(d); !errors.Is(err, wantErr) {
		t.Fatalf("first fetch err = %v, want %v", err, wantErr)
	}
	if _, err := copilotCatalog(d); !errors.Is(err, wantErr) {
		t.Fatalf("second fetch err = %v, want %v", err, wantErr)
	}
	if *calls != 1 {
		t.Errorf("copilotListModelsFn called %d times after a cached error, want 1", *calls)
	}
}

// pickCopilotModel lists only Enabled catalog models; a disabled one never appears as a pickable row.
func TestPickCopilotModelHidesDisabledModels(t *testing.T) {
	reg := menuTestEnv(t)
	fakeCopilotBin(t)
	stubCatalog(t, []copilotsdk.Model{
		{ID: "claude-sonnet-5", Name: "Claude Sonnet 5", Enabled: true},
		{ID: "retired-model", Name: "Retired Model", Enabled: false},
	}, nil)

	if _, ok := reg.Providers["github-copilot"]; !ok {
		t.Skip("no github-copilot provider in the registry")
	}

	in := strings.NewReader("\n") // blank = cancel; we only need the printed row list
	var out bytes.Buffer
	d := &dialog{reg: reg, o: Options{In: in}, br: bufio.NewReader(in), out: &out}
	pickCopilotModel(d)

	shown := out.String()
	if !strings.Contains(shown, "claude-sonnet-5") {
		t.Errorf("enabled model missing from the picked rows:\n%s", shown)
	}
	if strings.Contains(shown, "retired-model") {
		t.Errorf("disabled model must not be listed:\n%s", shown)
	}
}

// When the SDK catalog call fails, pickCopilotModel falls back to opencode's own catalog (modelChoices)
// and names the fallback source, instead of leaving the Model row dead.
func TestPickCopilotModelFallsBackToOpencodeOnSDKError(t *testing.T) {
	reg := menuTestEnv(t)
	fakeCopilotBin(t)
	stubCatalog(t, nil, errors.New("rpc: not authenticated"))

	origChoices := modelChoices
	t.Cleanup(func() { modelChoices = origChoices })
	modelChoices = func(p *providers.Provider, claude bool) []string { return []string{"claude-opus-5.5"} }

	gp, ok := reg.Providers["github-copilot"]
	if !ok {
		t.Skip("no github-copilot provider in the registry")
	}
	wantOpus := gp.Models["opus"] // must survive untouched: the pick writes only the COPILOT_MODEL pin

	in := strings.NewReader("1\n")
	var out bytes.Buffer
	d := &dialog{reg: reg, o: Options{In: in}, br: bufio.NewReader(in), out: &out}
	pickCopilotModel(d)

	if !strings.Contains(out.String(), "opencode") {
		t.Errorf("output does not name the opencode fallback:\n%s", out.String())
	}
	if got, _ := providers.CopilotModel(reg); got != "claude-opus-5.5" {
		t.Errorf("COPILOT_MODEL resolver = %q, want claude-opus-5.5 (saved from the fallback list)", got)
	}
	if got := gp.Models["opus"]; got != wantOpus {
		t.Errorf("github-copilot opus slot = %q, want unchanged %q: the pick must not write the provider overlay", got, wantOpus)
	}
}

// copilotEffortRow lists only the catalog model's own supported efforts, with its DefaultEffort marked.
func TestCopilotEffortRowListsModelsOwnEfforts(t *testing.T) {
	menuTestEnv(t)
	fakeCopilotBin(t)
	stubCatalog(t, []copilotsdk.Model{
		{ID: "claude-sonnet-5", ReasoningEffort: true, Efforts: []string{"low", "medium", "high"}, DefaultEffort: "medium"},
	}, nil)

	d := &dialog{out: discardWriter{}}
	r := copilotEffortRow(d, "claude-sonnet-5")
	if r == nil {
		t.Fatal("copilotEffortRow = nil, want a row (model supports reasoning effort)")
	}
	in := strings.NewReader("\n") // blank = cancel; we only need the printed choice list
	var out bytes.Buffer
	d2 := &dialog{o: Options{In: in}, br: bufio.NewReader(in), out: &out}
	r2 := copilotEffortRow(d2, "claude-sonnet-5")
	r2.run()
	shown := out.String()
	if !strings.Contains(shown, "Medium (default)") {
		t.Errorf("output missing the default marker on medium:\n%s", shown)
	}
	if strings.Contains(shown, "Extra High") || strings.Contains(shown, "Max") {
		t.Errorf("output lists an effort the model does not support:\n%s", shown)
	}
}

// Switching to a model that cannot honor the current effort/context pins must clear them, with a reason.
func TestClearIneligibleCopilotPinsOnModelSwitch(t *testing.T) {
	menuTestEnv(t)
	fakeCopilotBin(t)
	stubCatalog(t, []copilotsdk.Model{
		{ID: "new-model", ReasoningEffort: true, Efforts: []string{"low", "medium"}, ContextTokens: 128_000},
	}, nil)
	if err := providers.SetPins(map[string]string{
		providers.PinCopilotEffort:  "high",
		providers.PinCopilotContext: "long_context",
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	d := &dialog{out: &out}
	clearIneligibleCopilotPins(d, "new-model")

	if got := providers.Pin(providers.PinCopilotEffort); got != "" {
		t.Errorf("effort pin = %q, want cleared (new-model doesn't support high)", got)
	}
	if got := providers.Pin(providers.PinCopilotContext); got != "" {
		t.Errorf("context pin = %q, want cleared (new-model has no long-context tier)", got)
	}
	if !strings.Contains(out.String(), "doesn't support that effort") || !strings.Contains(out.String(), "has no long-context tier") {
		t.Errorf("output missing the clear reasons:\n%s", out.String())
	}
}

// clearIneligibleCopilotPins must leave compatible pins alone: a model that still supports the pinned
// effort and still has a long-context tier keeps both.
func TestClearIneligibleCopilotPinsKeepsCompatiblePins(t *testing.T) {
	menuTestEnv(t)
	fakeCopilotBin(t)
	stubCatalog(t, []copilotsdk.Model{
		{ID: "still-fits", ReasoningEffort: true, Efforts: []string{"low", "high"}, ContextTokens: 1_000_000},
	}, nil)
	if err := providers.SetPins(map[string]string{
		providers.PinCopilotEffort:  "high",
		providers.PinCopilotContext: "long_context",
	}); err != nil {
		t.Fatal(err)
	}

	d := &dialog{out: discardWriter{}}
	clearIneligibleCopilotPins(d, "still-fits")

	if got := providers.Pin(providers.PinCopilotEffort); got != "high" {
		t.Errorf("effort pin = %q, want unchanged high", got)
	}
	if got := providers.Pin(providers.PinCopilotContext); got != "long_context" {
		t.Errorf("context pin = %q, want unchanged long_context", got)
	}
}

// Catalog-driven rows (the Model picker's name+multiplier line) must still fit 80 columns against the longest label.
func TestCopilotCatalogRowsFitEightyColumns(t *testing.T) {
	mult := 10.0
	id := "gemini-3-pro-preview" // a realistic worst-case id length from the live catalog
	desc := copilotModelDesc(copilotsdk.Model{ID: id, Name: "Gemini 3 Pro Preview", Multiplier: &mult}, id)
	if w := rowRenderedWidth([]string{id}, desc); w > 80 {
		t.Errorf("Model row = %d cols, want <=80: %q", w, desc)
	}
}
