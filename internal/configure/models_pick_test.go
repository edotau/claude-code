package configure

import (
	"bufio"
	"bytes"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/copilotsdk"
	"github.com/edotau/claude-code/internal/providers"
)

// copilot's Model row saves to the COPILOT_MODEL pin (pickCopilotModel): one model list, no tier step,
// and the github-copilot provider's own models table is left untouched (opencode's fallback ladder reads it).
// copilotPickerTitle is pickCopilotModel's numbered-prompt title, used to split repeated renders.
const copilotPickerTitle = "copilot — model (SDK catalog)"

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
	in := strings.NewReader("1\n") // numbered pick, non-TTY: row 1 = the first model id, no tier prompt first
	d2.o, d2.br = Options{In: in}, bufio.NewReader(in)
	if !r.run() {
		t.Fatal("copilot Model row run() = false")
	}
	if got, _ := providers.CopilotModel(reg); got != "claude-opus-5.5" {
		t.Errorf("COPILOT_MODEL resolver = %q, want claude-opus-5.5 (one pick, no tier step)", got)
	}
	if got := gp.Models["opus"]; got != wantOpus {
		t.Errorf("github-copilot opus slot = %q, want unchanged %q: copilot's Model row must not write the provider", got, wantOpus)
	}
	if got := gp.Models["sonnet"]; got != wantSonnet {
		t.Errorf("github-copilot sonnet slot = %q, want unchanged %q: copilot's Model row must not write other slots", got, wantSonnet)
	}
}

// runCopilotPicker pins COPILOT_MODEL to modelID (so Effort/Context act on the stub model), then drives
// pickCopilotModel with the newline-separated answers and returns everything it printed.
func runCopilotPicker(t *testing.T, reg *providers.Registry, modelID, answers string) string {
	t.Helper()
	if err := providers.SetPins(map[string]string{providers.PinCopilotModel: modelID}); err != nil {
		t.Fatal(err)
	}
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
	stubCatalog(t, []copilotsdk.Model{
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
	stubCatalog(t, []copilotsdk.Model{
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
	stubCatalog(t, []copilotsdk.Model{
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
var effortStub = []copilotsdk.Model{
	{ID: "stub-model-1", Enabled: true, ReasoningEffort: true, Efforts: []string{"low", "high"}, DefaultEffort: "low"},
}

// Picking Effort opens its sub-picker; the chosen effort is pinned and the re-shown picker's Effort desc shows it.
func TestPickCopilotModelEffortSubPickSetsPinAndRerendersDesc(t *testing.T) {
	reg := menuTestEnv(t)
	fakeCopilotBin(t)
	stubCatalog(t, effortStub, nil)

	shown := runCopilotPicker(t, reg, "stub-model-1", "1\n3\n\n")
	if got := providers.Pin(providers.PinCopilotEffort); got != "high" {
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
	if err := providers.SetPins(map[string]string{providers.PinCopilotEffort: "high"}); err != nil {
		t.Fatal(err)
	}

	runCopilotPicker(t, reg, "stub-model-1", "1\n1\n\n")
	if got := providers.Pin(providers.PinCopilotEffort); got != "" {
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
	if got := providers.Pin(providers.PinCopilotEffort); got != "high" {
		t.Errorf("effort pin = %q, want high (stub-model-1 supports it)", got)
	}
}

// The picker's Effort and Context rows must fit 80 columns against their own labels.
func TestPickCopilotModelRowsFitEightyColumns(t *testing.T) {
	menuTestEnv(t)
	fakeCopilotBin(t)
	stubCatalog(t, []copilotsdk.Model{
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
