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

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
)

// stubCodexCatalog replaces codexListModelsFn with a fixed result and restores it on cleanup.
func stubCodexCatalog(t *testing.T, list []codexModelInfo, err error) {
	t.Helper()
	orig := codexListModelsFn
	codexListModelsFn = func(ctx context.Context) ([]codexModelInfo, error) { return list, err }
	t.Cleanup(func() { codexListModelsFn = orig })
}

// stubGeminiCatalog replaces geminiListModelsFn with a fixed result and restores it on cleanup.
func stubGeminiCatalog(t *testing.T, list []geminiModelInfo, err error) {
	t.Helper()
	orig := geminiListModelsFn
	geminiListModelsFn = func(ctx context.Context) ([]geminiModelInfo, error) { return list, err }
	t.Cleanup(func() { geminiListModelsFn = orig })
}

// fakeVendorBin writes an executable script to paths.BinDir()/bin, so paths.VendorBin(name) resolves and
// codexListModelsFn/geminiListModelsFn's own exec.CommandContext call runs it — exercising the real JSON/TSV
// parsing path, not the test seam. Must run after menuTestEnv, which pins HOME/the config dir this writes under.
func fakeVendorBin(t *testing.T, bin, script string) {
	t.Helper()
	dir := paths.BinDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, bin), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// --- codex JSON parse (the real codexListModelsFn, not the seam) -----------------------------

const codexDebugModelsJSON = `{"models":[
  {"slug":"gpt-6.1-sol","display_name":"GPT-6.1-Sol","visibility":"list","context_window":500000,
   "default_reasoning_level":"medium","supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"},{"effort":"high"}]},
  {"slug":"gpt-6-hidden","display_name":"Hidden","visibility":"hidden","context_window":100000,
   "supported_reasoning_levels":[{"effort":"low"}]}
]}`

func TestCodexListModelsFnParsesJSONAndDropsNonListVisibility(t *testing.T) {
	menuTestEnv(t)
	fakeVendorBin(t, "codex", "echo '"+codexDebugModelsJSON+"'")

	list, err := codexListModelsFn(context.Background())
	if err != nil {
		t.Fatalf("codexListModelsFn: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("list = %v, want exactly 1 entry (visibility!=list dropped)", list)
	}
	m := list[0]
	if m.Slug != "gpt-6.1-sol" || m.DisplayName != "GPT-6.1-Sol" {
		t.Errorf("m = %+v, want slug/display_name carried through", m)
	}
	if m.ContextWindow != 500000 {
		t.Errorf("ContextWindow = %d, want 500000", m.ContextWindow)
	}
	if m.DefaultEffort != "medium" {
		t.Errorf("DefaultEffort = %q, want medium", m.DefaultEffort)
	}
	if got := strings.Join(m.Efforts, ","); got != "low,medium,high" {
		t.Errorf("Efforts = %q, want low,medium,high", got)
	}
}

func TestCodexListModelsFnErrorsWhenCodexUnreachable(t *testing.T) {
	menuTestEnv(t) // no bin/codex written: paths.VendorBin must fail before ever exec'ing anything
	if _, err := codexListModelsFn(context.Background()); err == nil {
		t.Fatal("codexListModelsFn = nil error, want VendorBin's not-installed error")
	}
}

// --- gemini TSV parse (the real geminiListModelsFn, not the seam) ----------------------------

func TestGeminiListModelsFnParsesTSVSkipsFetchingLineAndGroupsByBase(t *testing.T) {
	menuTestEnv(t)
	tsv := "Fetching models...\n" +
		"gemini-3.8-flash-low\tGemini 3.8 Flash (Low)\n" +
		"gemini-3.8-flash-medium\tGemini 3.8 Flash (Medium)\n" +
		"gemini-3.8-flash-high\tGemini 3.8 Flash (High)\n" +
		"gemini-2.5-pro\tGemini 2.5 Pro\n"
	fakeVendorBin(t, "gemini", "printf '"+strings.ReplaceAll(tsv, "\n", `\n`)+"'")

	list, err := geminiListModelsFn(context.Background())
	if err != nil {
		t.Fatalf("geminiListModelsFn: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("list = %+v, want 2 bases (flash, pro), Fetching line never a base", list)
	}
	flash := geminiFindBase(list, "gemini-3.8-flash")
	if flash == nil {
		t.Fatalf("list = %+v, want a gemini-3.8-flash base", list)
	}
	if got := strings.Join(flash.Efforts, ","); got != "low,medium,high" {
		t.Errorf("flash.Efforts = %q, want low,medium,high (variant ids grouped under one base)", got)
	}
	if flash.Name != "Gemini 3.8 Flash" {
		t.Errorf("flash.Name = %q, want the effort suffix trimmed off", flash.Name)
	}
	pro := geminiFindBase(list, "gemini-2.5-pro")
	if pro == nil || len(pro.Efforts) != 0 {
		t.Errorf("pro = %+v, want a base with no effort variants", pro)
	}
}

func TestGeminiListModelsFnErrorsWhenAgyUnreachable(t *testing.T) {
	menuTestEnv(t) // no bin/agy written
	if _, err := geminiListModelsFn(context.Background()); err == nil {
		t.Fatal("geminiListModelsFn = nil error, want VendorBin's not-installed error")
	}
}

// --- codex picker: Effort first, then the catalog ---------------------------------------------

var codexStub = []codexModelInfo{
	{Slug: "gpt-6.1-sol", DisplayName: "GPT-6.1-Sol", Efforts: []string{"low", "medium", "high"}, DefaultEffort: "medium", ContextWindow: 500_000},
}

// runCodexPicker pins CODEX_MODEL to slug (so Effort acts on the stub model), then drives pickCodexModel
// with the newline-separated answers and returns everything printed.
func runCodexPicker(t *testing.T, slug, answers string) string {
	t.Helper()
	if err := providers.SetPins(map[string]string{providers.PinCodexModel: slug}); err != nil {
		t.Fatal(err)
	}
	in := strings.NewReader(answers)
	var out bytes.Buffer
	d := &dialog{o: Options{In: in}, br: bufio.NewReader(in), out: &out}
	pickCodexModel(d)
	return out.String()
}

func TestPickCodexModelShowsEffortRowFirstWithDefaultMarked(t *testing.T) {
	menuTestEnv(t)
	stubCodexCatalog(t, codexStub, nil)

	shown := runCodexPicker(t, "gpt-6.1-sol", "\n")
	iEffort, iModel := strings.Index(shown, "1) Effort "), strings.Index(shown, "2) gpt-6.1-sol")
	if iEffort < 0 || iModel < 0 || iEffort > iModel {
		t.Fatalf("rows = Effort@%d model@%d, want Effort row 1, model row 2:\n%s", iEffort, iModel, shown)
	}
	line := shown[iEffort : iEffort+strings.Index(shown[iEffort:], "\n")]
	if !strings.Contains(line, "default (medium)") {
		t.Errorf("Effort row = %q, want desc containing default (medium)", line)
	}
}

func TestPickCodexModelHidesEffortForNoEffortModel(t *testing.T) {
	menuTestEnv(t)
	stubCodexCatalog(t, []codexModelInfo{{Slug: "plain-model", DisplayName: "Plain"}}, nil)

	shown := runCodexPicker(t, "plain-model", "\n")
	if strings.Contains(shown, ") Effort ") {
		t.Errorf("Effort row shown for a model without reasoning levels:\n%s", shown)
	}
	if !strings.Contains(shown, "1) plain-model") {
		t.Errorf("model row missing (want row 1):\n%s", shown)
	}
}

func TestPickCodexModelEffortSubPickSetsPin(t *testing.T) {
	menuTestEnv(t)
	stubCodexCatalog(t, codexStub, nil)

	runCodexPicker(t, "gpt-6.1-sol", "1\n4\n\n") // Effort row, then row 4 of [unset,low,medium,high] -> high
	if got := providers.Pin(providers.PinCodexEffort); got != "high" {
		t.Errorf("effort pin = %q, want high", got)
	}
}

func TestPickCodexModelFallbackLabelled(t *testing.T) {
	menuTestEnv(t)
	stubCodexCatalog(t, nil, errors.New("codex: debug models: exit status 1"))

	shown := runCodexPicker(t, "gpt-6-astra", "\n")
	if !strings.Contains(shown, "(fallback list)") {
		t.Errorf("output does not label the fallback list:\n%s", shown)
	}
	if !strings.Contains(shown, "gpt-6-astra") {
		t.Errorf("fallback rows missing the known fallback model:\n%s", shown)
	}
}

func TestPickCodexModelSavesModelAndClearsIneligibleEffort(t *testing.T) {
	menuTestEnv(t)
	if err := providers.SetPins(map[string]string{providers.PinCodexEffort: "high"}); err != nil {
		t.Fatal(err)
	}
	stubCodexCatalog(t, []codexModelInfo{
		{Slug: "gpt-6.1-sol", Efforts: []string{"low", "medium", "high"}, DefaultEffort: "low"},
		{Slug: "no-effort-model"},
	}, nil)

	// cur = gpt-6.1-sol, so row 1 = Effort, row 2 = gpt-6.1-sol, row 3 = no-effort-model.
	shown := runCodexPicker(t, "gpt-6.1-sol", "3\n")
	if !strings.Contains(shown, "model: codex = no-effort-model") {
		t.Errorf("save line missing:\n%s", shown)
	}
	if got, _ := providers.CodexModel(); got != "no-effort-model" {
		t.Errorf("CODEX_MODEL = %q, want no-effort-model", got)
	}
	if got := providers.Pin(providers.PinCodexEffort); got != "" {
		t.Errorf("effort pin = %q, want cleared (no-effort-model supports no effort at all)", got)
	}
}

func TestPickCodexModelRowsFitEightyColumns(t *testing.T) {
	menuTestEnv(t)
	d := &dialog{out: discardWriter{}}
	m := codexModelInfo{Slug: "gpt-6.1-sol", Efforts: []string{"low", "medium", "high", "xhigh", "max", "ultra"}, DefaultEffort: "ultra"}
	r := codexEffortRow(d, []codexModelInfo{m}, "gpt-6.1-sol")
	if r == nil {
		t.Fatal("codexEffortRow = nil, want a row")
	}
	if w := rowRenderedWidth([]string{"Effort", "gpt-6.1-sol"}, r.desc); w > 80 {
		t.Errorf("Effort row = %d cols, want <=80: %q", w, r.desc)
	}
}

// --- gemini picker: Effort first (for multi-effort bases), then one row per base --------------

var geminiStub = []geminiModelInfo{
	{Base: "gemini-3.8-flash", Name: "Gemini 3.8 Flash", Efforts: []string{"low", "medium", "high"}},
	{Base: "gemini-2.5-pro", Name: "Gemini 2.5 Pro"},
}

func runGeminiPicker(t *testing.T, pinID, answers string) string {
	t.Helper()
	if err := providers.SetPins(map[string]string{providers.PinGeminiModel: pinID}); err != nil {
		t.Fatal(err)
	}
	in := strings.NewReader(answers)
	var out bytes.Buffer
	d := &dialog{o: Options{In: in}, br: bufio.NewReader(in), out: &out}
	pickGeminiModel(d)
	return out.String()
}

func TestPickGeminiModelShowsEffortRowForMultiEffortBase(t *testing.T) {
	menuTestEnv(t)
	stubGeminiCatalog(t, geminiStub, nil)

	shown := runGeminiPicker(t, "gemini-3.8-flash-medium", "\n")
	iEffort, iModel := strings.Index(shown, "1) Effort "), strings.Index(shown, "2) gemini-3.8-flash")
	if iEffort < 0 || iModel < 0 || iEffort > iModel {
		t.Fatalf("rows = Effort@%d model@%d, want Effort row 1, model row 2:\n%s", iEffort, iModel, shown)
	}
}

func TestPickGeminiModelHidesEffortForSingleEffortBase(t *testing.T) {
	menuTestEnv(t)
	stubGeminiCatalog(t, geminiStub, nil)

	shown := runGeminiPicker(t, "gemini-2.5-pro", "\n")
	if strings.Contains(shown, ") Effort ") {
		t.Errorf("Effort row shown for a base with no effort variants:\n%s", shown)
	}
	if !strings.Contains(shown, "1) gemini-3.8-flash") {
		t.Errorf("model rows missing (want row 1 = gemini-3.8-flash):\n%s", shown)
	}
}

func TestPickGeminiModelSavesBaseEffortID(t *testing.T) {
	menuTestEnv(t)
	stubGeminiCatalog(t, geminiStub, nil)

	// cur base = gemini-2.5-pro (no effort row); pick row 1 = gemini-3.8-flash: picks an effort variant.
	shown := runGeminiPicker(t, "gemini-2.5-pro", "1\n")
	if !strings.Contains(shown, "model: gemini = gemini-3.8-flash-low") {
		t.Errorf("save line missing/wrong (want the first effort variant):\n%s", shown)
	}
	if got, _ := providers.GeminiModel(); got != "gemini-3.8-flash-low" {
		t.Errorf("GEMINI_AGENT_MODEL pin = %q, want gemini-3.8-flash-low", got)
	}
}

func TestPickGeminiModelFallbackLabelled(t *testing.T) {
	menuTestEnv(t)
	stubGeminiCatalog(t, nil, errors.New("agy: models: exit status 1"))

	shown := runGeminiPicker(t, "", "\n")
	if !strings.Contains(shown, "(fallback list)") {
		t.Errorf("output does not label the fallback list:\n%s", shown)
	}
}

func TestPickGeminiModelRowsFitEightyColumns(t *testing.T) {
	menuTestEnv(t)
	d := &dialog{out: discardWriter{}}
	stubGeminiCatalog(t, geminiStub, nil)
	r := geminiEffortRow(d, "gemini-3.8-flash", "medium")
	if r == nil {
		t.Fatal("geminiEffortRow = nil, want a row")
	}
	if w := rowRenderedWidth([]string{"Effort", "gemini-3.8-flash"}, r.desc); w > 80 {
		t.Errorf("Effort row = %d cols, want <=80: %q", w, r.desc)
	}
}

// geminiCurrent resolves GEMINI_AGENT_MODEL (base+effort split) if pinned, else vendors.AgyDefaultModel;
// an unrelated GEMINI_MODEL env var (the gemini-bridge sweep tool's own key) must never leak in here.
func TestGeminiCurrentUsesPinElseAgyDefaultIgnoringUnrelatedEnvVar(t *testing.T) {
	menuTestEnv(t)
	t.Setenv("GEMINI_MODEL", "should-never-be-read")

	base, effort := geminiCurrent()
	if base != "gemini-3.8-flash" || effort != "high" {
		t.Errorf("geminiCurrent() = (%q, %q), want vendors.AgyDefaultModel split (gemini-3.8-flash, high)", base, effort)
	}

	if err := providers.SetPins(map[string]string{providers.PinGeminiModel: "gemini-2.5-pro-low"}); err != nil {
		t.Fatal(err)
	}
	base, effort = geminiCurrent()
	if base != "gemini-2.5-pro" || effort != "low" {
		t.Errorf("geminiCurrent() pinned = (%q, %q), want (gemini-2.5-pro, low)", base, effort)
	}
}
