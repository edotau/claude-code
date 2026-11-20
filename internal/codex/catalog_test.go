package codex

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
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
	for _, m := range []string{"databricks-claude-opus-5-5", "databricks-gpt-6-1-terra", "databricks-deepseek-v4-1-flash"} {
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
