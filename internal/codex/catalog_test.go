package codex

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/serving"
)

func BenchmarkIndexCodexModels(b *testing.B) {
	var raw strings.Builder
	raw.WriteString(`{"models":[`)
	for i := range 64 {
		if i > 0 {
			raw.WriteByte(',')
		}
		raw.WriteString(`{"slug":"gpt-model-`)
		raw.WriteString(strconv.Itoa(i))
		raw.WriteString(`"}`)
	}
	raw.WriteString(`]}`)
	data := []byte(raw.String())
	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := indexCodexModels(data); err != nil {
			b.Fatal(err)
		}
	}
}

func TestRunCodexCatalogWrapsExitErrorWithTrimmedStderr(t *testing.T) {
	hosttest.SkipOnWindows(t, "sh scripts, symlinks or errno")
	home := t.TempDir()
	bin := filepath.Join(home, "codex")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '  bad catalog  \\n' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := runCodexCatalog(context.Background(), bin, home, "debug", "models", "--bundled")
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("expected wrapped exit status 1, got %v", err)
	}
	if got, want := err.Error(), "exit status 1: bad catalog"; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

// Given a stored catalog whose key matches, Then a launch neither spawns codex nor makes a temp home.
func TestCodexCatalogPathCacheHitSkipsSpawnAndTempHome(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HARNESS_VAULT_KEY", "probe-secret")
	spawns := filepath.Join(t.TempDir(), "spawns")
	bin := pathstest.FakeVendor(t, "codex", "echo run >> "+spawns+"\necho 'codex-cli 0.155.0'")
	p := &providers.Provider{Name: "dbx", Preset: providers.PresetDatabricks}
	_, key, err := catalogInputs(bin, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(paths.StateDir(), "codex")
	if err := storeCodexCatalog(dir, filepath.Join(dir, "catalog-dbx.json"), filepath.Join(dir, "catalog-dbx.key"), []byte(`{}`), key); err != nil {
		t.Fatal(err)
	}
	orig := catalogTempDir
	catalogTempDir = func() (string, error) { t.Error("temp home created on a cache hit"); return orig() }
	t.Cleanup(func() { catalogTempDir = orig })
	got, _, err := codexCatalogPath(context.Background(), p)
	if err != nil || got != filepath.Join(dir, "catalog-dbx.json") {
		t.Fatalf("path = %q, err = %v, want the cached catalog", got, err)
	}
	if raw, _ := os.ReadFile(spawns); len(raw) != 0 {
		t.Fatalf("codex spawned on a cache hit: %q", raw)
	}
}

// A typed model Responses cannot serve is refused with the model named and a Responses model suggested; no silent swap.
func TestCodexRefuseNamesModelAndSuggestsResponses(t *testing.T) {
	p := &providers.Provider{Name: "dbx", Preset: providers.PresetDatabricks, BaseURL: "https://dbx.test",
		Routes: map[string]string{providers.ProtocolResponses: "/responses", providers.ProtocolOpenAI: "/chat", providers.ProtocolAnthropic: "/anthropic"}}
	for _, m := range []string{"databricks-claude-opus-5-5", "databricks-gpt-oss-120b", "databricks-deepseek-v4-1-flash"} {
		err := codexRefuse(p, m)
		if err == nil || !strings.Contains(err.Error(), m) || !strings.Contains(err.Error(), "Responses") {
			t.Errorf("codexRefuse(%s) = %v, want a refusal naming the model", m, err)
		}
		if _, ok := err.(*agent.UsageError); !ok {
			t.Errorf("codexRefuse(%s) = %T, want *agent.UsageError (exit 2)", m, err)
		}
	}
	if err := codexRefuse(p, "databricks-gpt-6-sol"); err != nil {
		t.Errorf("codexRefuse(gpt-6-sol) = %v, want nil", err)
	}
	if err := codexRefuse(p, "databricks-gpt-6-luna"); err != nil {
		t.Errorf("codexRefuse(gpt-6-luna) = %v, want nil: a GPT row flagged Responses=false still runs over Responses", err)
	}
}

// Given GPT rows flagged Responses=false, Then the catalog lists the servable ones and not chat-only or Claude rows.
func TestCatalogListsServableGPTRowsWithoutResponsesFlag(t *testing.T) {
	raw := `{"models":[{"slug":"gpt-6.1-sol"},{"slug":"gpt-6-luna"},{"slug":"gpt-oss-120b"},{"slug":"claude-opus-5-5"}]}`
	p := &providers.Provider{Name: "dbx", Preset: providers.PresetDatabricks, ModelPrefix: "databricks-"}
	menu := []providers.OpencodeModel{{ID: "gpt-6-1-sol", Label: "Sol", Responses: true}, {ID: "claude-opus-5-5", Label: "Opus"},
		{ID: "gpt-oss-120b", Label: "OSS"}, {ID: "gpt-6-luna", Label: "Luna"}}
	body, _, err := buildCodexCatalog(p, menu, []byte(raw), nil)
	if err != nil || body == nil {
		t.Fatalf("build = %v, %v", body, err)
	}
	var cat struct {
		Models []struct{ Slug, Visibility string }
	}
	if err := json.Unmarshal(body, &cat); err != nil {
		t.Fatal(err)
	}
	var listed []string
	for _, m := range cat.Models {
		if m.Visibility == "list" {
			listed = append(listed, m.Slug)
		}
	}
	if want := []string{"databricks-gpt-6-1-sol", "databricks-gpt-6-luna"}; !slices.Equal(listed, want) {
		t.Fatalf("listed = %v, want %v", listed, want)
	}
}

func catalogDefaults(t *testing.T, prefs map[string]string) (effort, verbosity string) {
	t.Helper()
	raw := `{"models":[{"slug":"gpt-6.1-sol","default_reasoning_level":"low","default_verbosity":"low","support_verbosity":true,
		"supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"},{"effort":"high"}]}]}`
	p := &providers.Provider{Name: "dbx", Preset: providers.PresetDatabricks}
	menu := []providers.OpencodeModel{{ID: "databricks-gpt-6-1-sol", Label: "Sol", Responses: true}}
	body, _, err := buildCodexCatalog(p, menu, []byte(raw), prefs)
	if err != nil || body == nil {
		t.Fatalf("build = %v, %v", body, err)
	}
	var cat struct {
		Models []struct {
			Effort    string `json:"default_reasoning_level"`
			Verbosity string `json:"default_verbosity"`
		}
	}
	if err := json.Unmarshal(body, &cat); err != nil {
		t.Fatal(err)
	}
	return cat.Models[0].Effort, cat.Models[0].Verbosity
}

// Given a configured effort and verbosity, Then a model whose vendor default is lower follows them when supported.
func TestCatalogFollowsConfiguredPreferences(t *testing.T) {
	if e, v := catalogDefaults(t, map[string]string{codexEffortKey: "medium", extraVerbosity: "medium"}); e != "medium" || v != "medium" {
		t.Errorf("defaults = %s/%s, want medium/medium", e, v)
	}
	if e, v := catalogDefaults(t, map[string]string{codexEffortKey: "max"}); e != "low" || v != "low" {
		t.Errorf("unsupported effort: defaults = %s/%s, want the vendor low/low", e, v)
	}
	if e, v := catalogDefaults(t, nil); e != "low" || v != "low" {
		t.Errorf("no preference: defaults = %s/%s, want the vendor low/low", e, v)
	}
}

func TestCatalogKeyFollowsPreferences(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HARNESS_VAULT_KEY", "probe-secret")
	bin := pathstest.FakeVendor(t, "codex", "echo 'codex-cli 0.155.0'")
	p := &providers.Provider{Name: "dbx", Preset: providers.PresetDatabricks}
	_, none, _ := catalogInputs(bin, p, nil)
	_, eff, _ := catalogInputs(bin, p, map[string]string{codexEffortKey: "medium"})
	_, verb, _ := catalogInputs(bin, p, map[string]string{extraVerbosity: "medium"})
	if none == eff || none == verb || eff == verb {
		t.Fatalf("keys must differ per preference: %s %s %s", none, eff, verb)
	}
}

type catalogRow struct {
	Slug, Visibility string
	ContextWindow    int                       `json:"context_window"`
	Levels           []struct{ Effort string } `json:"supported_reasoning_levels"`
}

func buildRows(t *testing.T, raw string, menu []providers.OpencodeModel) ([]catalogRow, string) {
	t.Helper()
	var warn strings.Builder
	defer agentconfig.SetCatalogWarn(agentconfig.SetCatalogWarn(&warn))
	p := &providers.Provider{Name: "dbx", Preset: providers.PresetDatabricks, ModelPrefix: "databricks-"}
	body, _, err := buildCodexCatalog(p, menu, []byte(raw), nil)
	if err != nil || body == nil {
		t.Fatalf("build = %v, %v", body, err)
	}
	var cat struct{ Models []catalogRow }
	if err := json.Unmarshal(body, &cat); err != nil {
		t.Fatal(err)
	}
	return cat.Models, warn.String()
}

const fallbackBundled = `{"models":[{"slug":"gpt-5.5","context_window":272000,"max_context_window":272000,"default_reasoning_level":"medium",
	"supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"},{"effort":"high"}]},{"slug":"gpt-6-luna"}]}`

// Given servable rows with no bundled base, Then they list after the based rows from the nearest GPT template.
func TestCatalogFallbackRowsFollowBasedRows(t *testing.T) {
	menu := []providers.OpencodeModel{{ID: "gpt-5-4", Label: "G54"}, {ID: "gpt-5-5", Label: "G55"}, {ID: "grok-4-7", Label: "Grok"},
		{ID: "kimi-k3", Label: "Kimi"}, {ID: "claude-sonnet-5-5", Label: "Sonnet"}, {ID: "gemini-4-argon", Label: "Gem"}, {ID: "gpt-oss-120b", Label: "OSS"}}
	rows, _ := buildRows(t, fallbackBundled, menu)
	var listed []string
	byslug := map[string]catalogRow{}
	for _, r := range rows {
		byslug[r.Slug] = r
		if r.Visibility == "list" {
			listed = append(listed, r.Slug)
		}
	}
	if want := []string{"databricks-gpt-5-5", "databricks-gpt-5-4"}; !slices.Equal(listed, want) {
		t.Fatalf("listed = %v, want %v", listed, want)
	}
	if got := byslug["databricks-gpt-5-4"]; got.ContextWindow != models.ContextWindowUnknown || len(got.Levels) != 3 {
		t.Errorf("gpt-5-4 = %+v, want window %d and the template's 3 levels", got, models.ContextWindowUnknown)
	}
}

// Given a servable row whose limit the catalog knows, Then its window is that limit and no assumption is noted.
func TestCatalogFallbackRowUsesKnownLimit(t *testing.T) {
	rows, warn := buildRows(t, fallbackBundled, []providers.OpencodeModel{{ID: "gpt-5-5", Label: "G"}, {ID: "gemini-4-argon", Label: "Gem", Responses: true}})
	if got := rows[1]; got.Slug != "databricks-gemini-4-argon" || got.ContextWindow != 1048576 || warn != "" {
		t.Errorf("row = %+v note %q, want window 1048576 and no note", got, warn)
	}
}

// Given a servable model with no template at all, Then the skip note names every such id on one line.
func TestCatalogSkipNoteNamesEveryUnlistedModel(t *testing.T) {
	p := &providers.Provider{Name: "dbx", Preset: providers.PresetDatabricks, ModelPrefix: "databricks-"}
	menu := []providers.OpencodeModel{{ID: "gpt-5-4"}, {ID: "grok-4-7"}}
	body, missing, err := buildCodexCatalog(p, menu, []byte(`{"models":[{"slug":"gpt-daybreak"}]}`), nil)
	if body != nil || err != nil || missing != "gpt-5-4" {
		t.Errorf("build = %s, %q, %v; want no body and only the GPT id", body, missing, err)
	}
}

// listFacts swaps the listing seam for the test; the id spelling is the wire name.
func listFacts(t *testing.T, facts map[string]serving.EndpointFacts) {
	t.Helper()
	serving.SetFactsForTest(t, func(_, wire string) (serving.EndpointFacts, bool) { f, ok := facts[wire]; return f, ok })
}

// Given a fresh listing, Then api_types decide Responses for a listed id, the old rule covers an unlisted one, Grok stays out.
func TestCodexServesFollowsListingAPITypes(t *testing.T) {
	responses := []string{"mlflow/v1/chat/completions", "openai/v1/responses", "codex/v1/responses"}
	listFacts(t, map[string]serving.EndpointFacts{
		"databricks-gpt-6-luna":  {APITypes: []string{"mlflow/v1/chat/completions", "anthropic/v1/messages"}},
		"databricks-kimi-k3":     {APITypes: responses},
		"databricks-grok-4-7":    {APITypes: responses},
		"databricks-gpt-5-4":     {Context: 400000},
		"databricks-gpt-oss-20b": {APITypes: []string{"mlflow/v1/chat/completions"}},
	})
	p := &providers.Provider{Name: "dbx", Preset: providers.PresetDatabricks, ModelPrefix: "databricks-"}
	for id, want := range map[string]bool{"gpt-6-luna": false, "kimi-k3": true, "grok-4-7": false, "gpt-5-4": true, "gpt-6-sol": true} {
		if got := codexServes(p, providers.OpencodeModel{ID: id}); got != want {
			t.Errorf("codexServes(%s) = %v, want %v", id, got, want)
		}
	}
	if err := codexRefuse(p, "databricks-gpt-6-luna"); err == nil {
		t.Error("codexRefuse must refuse a GPT id whose listing api_types lack openai/v1/responses")
	}
	if err := codexRefuse(p, "databricks-grok-4-7"); err == nil {
		t.Error("codexRefuse must keep refusing Grok although api_types lists Responses")
	}
}

// Given a fallback row whose listing states a window, Then the row uses it and the assumption note omits the id.
func TestCatalogFallbackRowUsesListedWindow(t *testing.T) {
	listFacts(t, map[string]serving.EndpointFacts{"databricks-gpt-5-4": {Context: 400000, Output: 128000}})
	menu := []providers.OpencodeModel{{ID: "gpt-5-5", Label: "G55"}, {ID: "gpt-5-4", Label: "G54"}, {ID: "gpt-6-luna", Label: "L"}}
	rows, warn := buildRows(t, fallbackBundled, menu)
	for _, r := range rows {
		if r.Slug == "databricks-gpt-5-4" && r.ContextWindow != 400000 {
			t.Errorf("gpt-5-4 window = %d, want the listed 400000", r.ContextWindow)
		}
	}
	if strings.Contains(warn, "gpt-5-4") {
		t.Errorf("note = %q, want gpt-5-4 absent once its window is known", warn)
	}
}
