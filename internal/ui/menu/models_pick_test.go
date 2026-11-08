package menu

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/copilot"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// copilot's Model row saves to the COPILOT_MODEL pin (pickCopilotModel): one model list, no alias step,
// and the github-copilot provider's own models table is left untouched (opencode's fallback ladder reads it).
// copilotPickerTitle is pickCopilotModel's numbered-prompt title, used to split repeated renders.
const copilotPickerTitle = "copilot — model (github · copilot catalog)"

func TestPickerChoicesSavedErrorUsesDialogOutput(t *testing.T) {
	reg := menuTestEnv(t)
	p := reg.Providers["max"]
	if _, err := providers.AppendPickerModels(p, []providers.ModelInfo{{ID: "saved-model"}}); err != nil {
		t.Fatal(err)
	}
	if err := vault.Update(func(tx *vault.Tx) error {
		records, err := tx.Records()
		if err != nil {
			return err
		}
		for name, record := range records {
			if strings.HasPrefix(name, "picker-additions/v1/") {
				record.Sealed = []byte("invalid-sealed-record")
				return tx.PutRecord(name, record)
			}
		}
		return errors.New("picker additions record not created")
	}); err != nil {
		t.Fatal(err)
	}
	_, wantErr := providers.PickerModels(p)
	if wantErr == nil {
		t.Fatal("corrupt saved additions must fail to read")
	}
	orig := modelChoices
	t.Cleanup(func() { modelChoices = orig })
	modelChoices = func(*providers.Provider, bool) []string { return []string{"live-model"} }
	var out bytes.Buffer
	got := pickerChoices(&dialog{out: &out}, p, true)
	if !slices.Equal(got, []string{"live-model"}) {
		t.Errorf("choices = %v, want live catalog preserved", got)
	}
	if want := "models: " + wantErr.Error() + "\n"; out.String() != want {
		t.Errorf("dialog output = %q, want %q", out.String(), want)
	}
}

func TestModelRowCopilotSkipsTierPicker(t *testing.T) {
	reg := menuTestEnv(t)
	origChoices := modelChoices
	t.Cleanup(func() { modelChoices = origChoices })
	modelChoices = func(p *providers.Provider, claude bool) []string {
		return []string{"claude-opus-5.5", "claude-sonnet-5"}
	}

	gp, ok := reg.Providers["personal"]
	if !ok {
		t.Skip("no personal (github) profile in the registry")
	}
	wantOpus, wantSonnet := gp.Models["opus"], gp.Models["sonnet"] // must survive untouched

	d2 := &dialog{reg: reg, out: discardWriter{}}
	r := modelRow(d2, "copilot")
	if r == nil {
		t.Fatal("modelRow(copilot) = nil, want the Model row")
	}
	in := strings.NewReader("1\n") // numbered pick, non-TTY: row 1 = the first model id, no alias prompt first
	d2.o, d2.br = Options{In: in}, bufio.NewReader(in)
	if !r.run() {
		t.Fatal("copilot Model row run() = false")
	}
	if got, _ := providers.CopilotModel(reg); got != "claude-opus-5.5" {
		t.Errorf("COPILOT_MODEL resolver = %q, want claude-opus-5.5 (one pick, no alias step)", got)
	}
	if got := gp.Models["opus"]; got != wantOpus {
		t.Errorf("github-copilot opus alias = %q, want unchanged %q: copilot's Model row must not write the provider", got, wantOpus)
	}
	if got := gp.Models["sonnet"]; got != wantSonnet {
		t.Errorf("github-copilot sonnet alias = %q, want unchanged %q: copilot's Model row must not write other aliases", got, wantSonnet)
	}
}

// runCopilotPicker pins COPILOT_MODEL to modelID (so Effort/Context act on the stub model), then drives
// pickCopilotModel with the newline-separated answers and returns everything it printed.
func runCopilotPicker(t *testing.T, reg *providers.Registry, modelID, answers string) string {
	t.Helper()
	putRec(t, "copilot", "MODEL", modelID)
	in := strings.NewReader(answers)
	var out bytes.Buffer
	d := &dialog{reg: reg, o: Options{In: in}, br: bufio.NewReader(in), out: &out}
	pickCopilotModel(d)
	return out.String()
}

// The Model picker lists Effort then Context (both acting on the current model) before any model row.
func TestPickCopilotModelShowsEffortAndContextRowsFirst(t *testing.T) {
	reg := menuTestEnv(t)
	fakeCopilotBin(t)
	stubCatalog(t, []copilot.SDKModel{
		{ID: "stub-model-1", Enabled: true, ReasoningEffort: true, Efforts: []string{"low", "medium"},
			DefaultEffort: "medium", ContextTokens: 2_000_000},
	}, nil)

	shown := runCopilotPicker(t, reg, "stub-model-1", "\n")
	iEffort, iContext, iModel := strings.Index(shown, "1) Effort "), strings.Index(shown, "2) Context "), strings.Index(shown, "3) stub-model-1")
	if iEffort < 0 || iContext < 0 || iModel < 0 {
		t.Fatalf("rows = Effort@%d Context@%d model@%d, want rows 1-3 = Effort, Context, stub-model-1:\n%s", iEffort, iContext, iModel, shown)
	}
	if iEffort >= iContext || iContext >= iModel {
		t.Errorf("row order = Effort@%d Context@%d model@%d, want Effort < Context < model", iEffort, iContext, iModel)
	}
	effortLine := shown[iEffort : iEffort+strings.Index(shown[iEffort:], "\n")]
	if !strings.Contains(effortLine, "default (medium)") {
		t.Errorf("Effort row = %q, want desc containing default (medium)", effortLine)
	}
	contextLine := shown[iContext : iContext+strings.Index(shown[iContext:], "\n")]
	if !strings.Contains(contextLine, "auto") {
		t.Errorf("Context row = %q, want desc containing auto", contextLine)
	}
}

// A model without reasoning-effort support gets no Effort row; its own model row still shows.
func TestPickCopilotModelHidesEffortForNoEffortModel(t *testing.T) {
	reg := menuTestEnv(t)
	fakeCopilotBin(t)
	stubCatalog(t, []copilot.SDKModel{
		{ID: "plain-model", Name: "Plain Model", Enabled: true, ReasoningEffort: false, ContextTokens: 100_000},
	}, nil)

	shown := runCopilotPicker(t, reg, "plain-model", "\n")
	if strings.Contains(shown, ") Effort ") {
		t.Errorf("Effort row shown for a model without reasoning effort:\n%s", shown)
	}
	if !strings.Contains(shown, "1) plain-model") {
		t.Errorf("model row missing (want row 1 = plain-model):\n%s", shown)
	}
}

// A model at or below the 200K baseline gets no Context row, while its Effort row still shows.
func TestPickCopilotModelHidesContextForIneligibleModel(t *testing.T) {
	reg := menuTestEnv(t)
	fakeCopilotBin(t)
	stubCatalog(t, []copilot.SDKModel{
		{ID: "small-window", Name: "Small Window", Enabled: true, ReasoningEffort: true,
			Efforts: []string{"low", "high"}, DefaultEffort: "low", ContextTokens: 50_000},
	}, nil)

	shown := runCopilotPicker(t, reg, "small-window", "\n")
	if !strings.Contains(shown, "1) Effort ") {
		t.Errorf("Effort row missing (want row 1):\n%s", shown)
	}
	if strings.Contains(shown, ") Context ") {
		t.Errorf("Context row shown for a model with no long-context tier:\n%s", shown)
	}
	if !strings.Contains(shown, "2) small-window") {
		t.Errorf("model row missing (want row 2 = small-window):\n%s", shown)
	}
}

// effortStub is a model with Effort (low/high, default low) and no Context row: Effort = row 1, model = row 2;
// in the Effort sub-picker, 1 = unset, 2 = Low, 3 = High.
var effortStub = []copilot.SDKModel{
	{ID: "stub-model-1", Enabled: true, ReasoningEffort: true, Efforts: []string{"low", "high"}, DefaultEffort: "low"},
}

// Picking Effort opens its sub-picker; the chosen effort is pinned and the re-shown picker's Effort desc shows it.
func TestPickCopilotModelEffortSubPickSetsPinAndRerendersDesc(t *testing.T) {
	reg := menuTestEnv(t)
	fakeCopilotBin(t)
	stubCatalog(t, effortStub, nil)

	shown := runCopilotPicker(t, reg, "stub-model-1", "1\n3\n\n")
	if got := vault.GetAgentRecord("copilot").Effort; got != "high" {
		t.Errorf("effort pin = %q, want high", got)
	}
	renders := strings.Split(shown, copilotPickerTitle)
	if len(renders) != 3 {
		t.Fatalf("picker rendered %d times, want 2 (re-shown after the Effort sub-pick):\n%s", len(renders)-1, shown)
	}
	if first := renders[1]; !strings.Contains(first, "[default (low)]") {
		t.Errorf("first render Effort desc missing [default (low)]:\n%s", first)
	}
	second := renders[2]
	if !strings.Contains(second, "[high]") || strings.Contains(second, "default (low)") {
		t.Errorf("second render Effort desc = want [high], not default (low):\n%s", second)
	}
}

// Picking "unset" in the Effort sub-picker clears an existing effort pin.
func TestPickCopilotModelEffortSubPickDefaultClearsPin(t *testing.T) {
	reg := menuTestEnv(t)
	fakeCopilotBin(t)
	stubCatalog(t, effortStub, nil)
	putRec(t, "copilot", "EFFORT", "high")

	runCopilotPicker(t, reg, "stub-model-1", "1\n1\n\n")
	if got := vault.GetAgentRecord("copilot").Effort; got != "" {
		t.Errorf("effort pin = %q, want cleared", got)
	}
}

// After an Effort sub-pick, picking the model row saves COPILOT_MODEL and keeps the (supported) effort pin.
func TestPickCopilotModelSavesModelAfterSettingEffort(t *testing.T) {
	reg := menuTestEnv(t)
	fakeCopilotBin(t)
	stubCatalog(t, effortStub, nil)

	// cur = the stub so its Effort row (row 1) offers low/high; after the sub-pick the model row is row 2.
	shown := runCopilotPicker(t, reg, "stub-model-1", "1\n3\n2\n")
	if !strings.Contains(shown, "model: copilot = stub-model-1") {
		t.Errorf("saveAgentModel not reached (no save line):\n%s", shown)
	}
	if got, _ := providers.CopilotModel(reg); got != "stub-model-1" {
		t.Errorf("COPILOT_MODEL resolver = %q, want stub-model-1", got)
	}
	if got := vault.GetAgentRecord("copilot").Effort; got != "high" {
		t.Errorf("effort pin = %q, want high (stub-model-1 supports it)", got)
	}
}

// The picker's Effort and Context rows must fit 80 columns against their own labels.
func TestPickCopilotModelRowsFitEightyColumns(t *testing.T) {
	menuTestEnv(t)
	fakeCopilotBin(t)
	stubCatalog(t, []copilot.SDKModel{
		{ID: "gemini-3-pro-preview", Enabled: true, ReasoningEffort: true,
			Efforts: []string{"low", "medium", "high", "xhigh"}, DefaultEffort: "xhigh", ContextTokens: 2_000_000},
	}, nil)

	d := &dialog{out: discardWriter{}}
	effort, context := copilotEffortRow(d, "gemini-3-pro-preview"), copilotContextRow(d, "gemini-3-pro-preview")
	if effort == nil || context == nil {
		t.Fatalf("Effort row = %v, Context row = %v, want both non-nil", effort, context)
	}
	for _, r := range []*billingRow{effort, context} {
		if w := rowRenderedWidth([]string{"Effort", "Context"}, r.desc); w > 80 {
			t.Errorf("%s row = %d cols, want <=80: %q", r.label, w, r.desc)
		}
	}
}

// fakeCopilotBin makes paths.AgentBin("copilot") resolve, so copilotCatalog gets past that check and
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

// stubCatalog replaces copilotCatalogFn with a fixed result and restores it on cleanup.
func stubCatalog(t *testing.T, models []copilot.SDKModel, err error) *int {
	t.Helper()
	orig := copilotCatalogFn
	calls := 0
	copilotCatalogFn = func(context.Context) ([]copilot.SDKModel, error) {
		calls++
		return models, err
	}
	t.Cleanup(func() { copilotCatalogFn = orig })
	return &calls
}

// copilotCatalog fetches the SDK catalog once per dialog: a second call (from a different row) must hit
// the cache, not re-invoke the seam (re-spawning the copilot runtime on every row would be a real cost).
func TestCopilotCatalogFetchedOncePerDialog(t *testing.T) {
	menuTestEnv(t)
	fakeCopilotBin(t)
	calls := stubCatalog(t, []copilot.SDKModel{{ID: "claude-sonnet-5", Enabled: true}}, nil)

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
	stubCatalog(t, []copilot.SDKModel{
		{ID: "claude-sonnet-5", Name: "Claude Sonnet 5", Enabled: true},
		{ID: "retired-model", Name: "Retired Model", Enabled: false},
	}, nil)

	if _, ok := reg.Providers["personal"]; !ok {
		t.Skip("no personal (github) profile in the registry")
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

	gp, ok := reg.Providers["personal"]
	if !ok {
		t.Skip("no personal (github) profile in the registry")
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
		t.Errorf("github-copilot opus alias = %q, want unchanged %q: the pick must not write the provider overlay", got, wantOpus)
	}
}

// copilotEffortRow lists only the catalog model's own supported efforts, with its DefaultEffort marked.
func TestCopilotEffortRowListsModelsOwnEfforts(t *testing.T) {
	menuTestEnv(t)
	fakeCopilotBin(t)
	stubCatalog(t, []copilot.SDKModel{
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
	stubCatalog(t, []copilot.SDKModel{
		{ID: "new-model", ReasoningEffort: true, Efforts: []string{"low", "medium"}, ContextTokens: 128_000},
	}, nil)
	putRec(t, "copilot", "EFFORT", "high")
	putRec(t, "copilot", "CONTEXT", "long_context")

	var out bytes.Buffer
	d := &dialog{out: &out}
	clearIneligibleCopilotPins(d, "new-model")

	if got := vault.GetAgentRecord("copilot").Effort; got != "" {
		t.Errorf("effort pin = %q, want cleared (new-model doesn't support high)", got)
	}
	if got := vault.GetAgentRecord("copilot").Context; got != "" {
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
	stubCatalog(t, []copilot.SDKModel{
		{ID: "still-fits", ReasoningEffort: true, Efforts: []string{"low", "high"}, ContextTokens: 1_000_000},
	}, nil)
	putRec(t, "copilot", "EFFORT", "high")
	putRec(t, "copilot", "CONTEXT", "long_context")

	d := &dialog{out: discardWriter{}}
	clearIneligibleCopilotPins(d, "still-fits")

	if got := vault.GetAgentRecord("copilot").Effort; got != "high" {
		t.Errorf("effort pin = %q, want unchanged high", got)
	}
	if got := vault.GetAgentRecord("copilot").Context; got != "long_context" {
		t.Errorf("context pin = %q, want unchanged long_context", got)
	}
}

// Catalog-driven rows (the Model picker's name+multiplier line) must still fit 80 columns against the longest label.
func TestCopilotCatalogRowsFitEightyColumns(t *testing.T) {
	mult := 10.0
	id := "gemini-3-pro-preview" // a realistic worst-case id length from the live catalog
	desc := copilotModelDesc(copilot.SDKModel{ID: id, Name: "Gemini 3 Pro Preview", Multiplier: &mult}, id)
	if w := rowRenderedWidth([]string{id}, desc); w > 80 {
		t.Errorf("Model row = %d cols, want <=80: %q", w, desc)
	}
}

// kimiChoiceNumber is the number of the picker's databricks-kimi-k3 row, which must carry the tested marker.
func kimiChoiceNumber(t *testing.T, shown string) int {
	t.Helper()
	choice := 0
	for line := range strings.SplitSeq(shown, "\n") {
		var n int
		var id string
		if _, err := fmt.Sscanf(strings.TrimSpace(line), "%d) %s", &n, &id); err == nil && id == "databricks-kimi-k3" {
			choice = n
			if !strings.Contains(line, "tested") {
				t.Errorf("kimi row missing tested marker: %q", line)
			}
		}
	}
	if choice == 0 {
		t.Fatalf("numbered kimi row missing from picker:\n%s", shown)
	}
	return choice
}

func TestPickCopilotModelDatabricksStoresCanonicalID(t *testing.T) {
	reg := menuTestEnv(t)
	cat := copilot.CopilotCatalog{
		Databricks: true,
		Source:     "dev serving endpoints",
		Rows: []copilot.CopilotEndpoint{
			{ID: "databricks-kimi-k3", Tested: true},
			{ID: "databricks-gpt-6-1-sol"},
		},
	}
	orig := copilotSourceFn
	t.Cleanup(func() { copilotSourceFn = orig })
	copilotSourceFn = func(context.Context, *providers.Registry, string) (copilot.CopilotCatalog, error) {
		return cat, nil
	}

	in := strings.NewReader("\n")
	var out bytes.Buffer
	d := &dialog{reg: reg, o: Options{In: in}, br: bufio.NewReader(in), out: &out}
	pickCopilotModel(d)
	shown := out.String()
	for _, id := range []string{"databricks-kimi-k3", "databricks-gpt-6-1-sol"} {
		if !strings.Contains(shown, id) {
			t.Errorf("serving endpoint %q missing from picker:\n%s", id, shown)
		}
	}
	choice := kimiChoiceNumber(t, shown)
	in = strings.NewReader(fmt.Sprintf("%d\n", choice))
	out.Reset()
	d = &dialog{reg: reg, o: Options{In: in}, br: bufio.NewReader(in), out: &out}
	pickCopilotModel(d)
	if got, _ := providers.CopilotModel(reg); got != "kimi-k3" {
		t.Errorf("COPILOT_MODEL = %q, want kimi-k3; picker output:\n%s", got, out.String())
	}

	_, rows := appendDatabricksCopilotRows(cat, "kimi-k3", nil)
	if rows[0][0] != "databricks-kimi-k3" || !strings.Contains(rows[0][1], "current") {
		t.Errorf("canonical current model not marked on endpoint row: %v", rows[0])
	}
}

// opencode pinned to a registry profile picks that profile's models, not the hub ladder's opencode-go plan.
func TestModelTargetHonorsAgentPin(t *testing.T) {
	reg := menuTestEnv(t)
	pin := ""
	for name, p := range reg.Providers {
		if p.Protocol != "" && p.OpencodeID == "" {
			pin = name
			break
		}
	}
	if pin == "" {
		t.Skip("no registry provider to pin")
	}
	vaulttest.SetRecord(t, "opencode", "profile", pin)
	p, claude := modelTarget(&dialog{reg: reg, out: discardWriter{}}, "opencode")
	if claude || p == nil || p.Name != pin {
		t.Fatalf("modelTarget(opencode) = %v, claude=%v; want the pinned profile %q", p, claude, pin)
	}
}
