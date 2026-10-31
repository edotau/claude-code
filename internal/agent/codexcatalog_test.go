package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/settings"
)

func TestCodexCatalogArgsDatabricks(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	originalHome := os.Getenv("CODEX_HOME")
	var warnings bytes.Buffer
	oldWarn := catalogWarn
	catalogWarn = &warnings
	t.Cleanup(func() { catalogWarn = oldWarn })
	write := func(path, body string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	read := func(path string) []byte {
		t.Helper()
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	if err := os.MkdirAll(paths.BinDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(paths.BinDir(), "codex"), `#!/bin/sh
set -eu
[ "$(pwd -P)" = "$(cd "$CODEX_HOME" && pwd -P)" ] || exit 90 # macOS: TempDir is /var → /private/var
[ -z "$(cat)" ] || exit 91
case "$*" in
  --version)
    printf 'version|%s|%s\n' "$(cd "$CODEX_HOME" && pwd -P)" "$(pwd -P)" >> "$CLAUDE_CONFIG_DIR/calls"
    printf 'codex-test 1.0\n'
    ;;
  'debug models --bundled')
    printf 'bundled|%s|%s\n' "$(cd "$CODEX_HOME" && pwd -P)" "$(pwd -P)" >> "$CLAUDE_CONFIG_DIR/calls"
    cat <<'JSON'
{"models":[
  {"slug":"gpt-6.1-sol","display_name":"native sol","visibility":"hide","supported_in_api":false,"priority":77,
   "context_window":123456,"unknown":{"nested":[{"preserved":true}]},
   "service_tiers":["priority"],"additional_speed_tiers":["fast"],"upgrade":{"id":"next"},"availability_nux":{"text":"hello"}},
  {"slug":"gpt-5.5","display_name":"native 5.5","context_window":654321}
]}
JSON
    ;;
  *)
    [ "$#" = 4 ] && [ "$1" = '-c' ] && [ "$3" = debug ] && [ "$4" = models ] || exit 92
    [ "$2" = "model_catalog_json=\"$CODEX_HOME/catalog.json\"" ] || exit 93
    [ -s "$CODEX_HOME/catalog.json" ] || exit 94
    printf 'validation|%s|%s\n' "$(cd "$CODEX_HOME" && pwd -P)" "$(pwd -P)" >> "$CLAUDE_CONFIG_DIR/calls"
    [ ! -f "$CLAUDE_CONFIG_DIR/reject" ] || exit 1
    ;;
esac
`, 0o700)
	profile := dbxRow("qa", "databricks-")
	args := codexCatalogArgs(profile)
	path := filepath.Join(paths.StateDir(), "codex", "catalog-qa.json")
	keyPath := filepath.Join(paths.StateDir(), "codex", "catalog-qa.key")
	wantArgs := []string{"-c", "model_catalog_json=" + tomlQuote(path)}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Fatalf("args = %q, want %q; warnings: %s", args, wantArgs, &warnings)
	}
	if got, want := warnings.String(), "codex is missing metadata for gpt-6-astra; update codex\n"; got != want {
		t.Fatalf("warnings = %q, want %q", got, want)
	}
	body := read(path)
	var catalog struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(body, &catalog); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Models) != 4 {
		t.Fatalf("models = %d, want two listed models and two hidden aliases", len(catalog.Models))
	}
	for i, want := range []struct{ slug, label string }{
		{"databricks-gpt-6-1-sol", "GPT-6.1 Sol · qa"},
		{"databricks-gpt-5-5", "GPT 5.5 · qa"},
	} {
		model := catalog.Models[i]
		if model["slug"] != want.slug || model["display_name"] != want.label || model["priority"] != float64(i) ||
			model["visibility"] != "list" || model["supported_in_api"] != true {
			t.Errorf("listed model %d = %v", i, model)
		}
		for _, field := range []string{"service_tiers", "additional_speed_tiers"} {
			if values, ok := model[field].([]any); !ok || len(values) != 0 {
				t.Errorf("model %d %s = %v, want empty array", i, field, model[field])
			}
		}
		for _, field := range []string{"upgrade", "availability_nux"} {
			if value, ok := model[field]; !ok || value != nil {
				t.Errorf("model %d %s = %v, want explicit null", i, field, value)
			}
		}
	}
	wantUnknown := map[string]any{"nested": []any{map[string]any{"preserved": true}}}
	if catalog.Models[0]["context_window"] != float64(123456) ||
		catalog.Models[1]["context_window"] != float64(654321) ||
		!reflect.DeepEqual(catalog.Models[0]["unknown"], wantUnknown) {
		t.Errorf("native metadata changed: %v", catalog.Models[:2])
	}
	for i, slug := range []string{"gpt-6.1-sol", "gpt-5.5"} {
		alias := catalog.Models[i+2]
		if alias["slug"] != slug || alias["visibility"] != "hide" {
			t.Errorf("hidden alias %d = %v", i, alias)
		}
		copy := maps.Clone(alias)
		copy["slug"], copy["visibility"] = catalog.Models[i]["slug"], "list"
		if !reflect.DeepEqual(copy, catalog.Models[i]) {
			t.Errorf("hidden alias metadata differs: %v", alias)
		}
	}
	for target, mode := range map[string]os.FileMode{path: 0o600, keyPath: 0o600, filepath.Dir(path): 0o700} {
		info, err := os.Stat(target)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != mode {
			t.Errorf("permissions for %s = %o, want %o", target, info.Mode().Perm(), mode)
		}
	}
	checkCalls := func(want []string) {
		t.Helper()
		lines := strings.Split(strings.TrimSpace(string(read(filepath.Join(paths.ConfigDir(), "calls")))), "\n")
		if len(lines) != len(want) {
			t.Fatalf("calls = %v, want %v", lines, want)
		}
		var bundledHome string
		for i, line := range lines {
			parts := strings.Split(line, "|")
			if len(parts) != 3 || parts[0] != want[i] {
				t.Fatalf("call %d = %q, want %q", i, line, want[i])
			}
			if parts[1] == "" || parts[1] == originalHome || parts[1] != parts[2] {
				t.Errorf("call did not use an isolated home and working directory: %q", line)
			}
			if _, err := os.Stat(parts[1]); !os.IsNotExist(err) {
				t.Errorf("temporary home remains: %q, error %v", parts[1], err)
			}
			if parts[0] == "bundled" {
				bundledHome = parts[1]
			}
			if parts[0] == "validation" && parts[1] != bundledHome {
				t.Errorf("validation home = %q, bundled home = %q", parts[1], bundledHome)
			}
		}
	}
	checkCalls([]string{"version", "bundled", "validation"})
	warnings.Reset()
	if cached := codexCatalogArgs(profile); !reflect.DeepEqual(cached, args) {
		t.Fatalf("cached args = %q, want %q", cached, args)
	}
	if warnings.Len() != 0 {
		t.Errorf("cache warnings = %q", &warnings)
	}
	checkCalls([]string{"version", "bundled", "validation", "version"})
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(paths.ConfigDir(), "reject"), "", 0o600)
	warnings.Reset()
	if failed := codexCatalogArgs(profile); failed != nil {
		t.Errorf("failed validation returned %q", failed)
	}
	if got := warnings.String(); strings.Count(got, "\n") != 1 || !strings.HasPrefix(got, "codex catalog: validation failed:") {
		t.Errorf("validation warnings = %q, want one validation failure", got)
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Errorf("failed validation committed a cache key: %v", err)
	}
	if !bytes.Equal(read(path), body) {
		t.Error("failed validation changed the previous catalog")
	}
	checkCalls([]string{"version", "bundled", "validation", "version", "version", "bundled", "validation"})

	if err := os.Remove(filepath.Join(paths.ConfigDir(), "reject")); err != nil {
		t.Fatal(err)
	}
	profile.Auth.Type = providers.AuthNone
	target := providers.Target{Provider: profile, Model: "databricks-gpt-6-1-sol"}
	checkLaunch := func(wantCatalog string) {
		t.Helper()
		env, argv, err := schemaFor("codex").Configure(context.Background(), target, "")
		if err != nil {
			t.Fatal(err)
		}
		markers := envMap(env)
		if markers[settings.CodexProfileEnv] != profile.Name {
			t.Errorf("inherited profile = %q, want %q", markers[settings.CodexProfileEnv], profile.Name)
		}
		catalog, present := markers[settings.CodexCatalogEnv]
		if !present || catalog != wantCatalog {
			t.Errorf("inherited catalog = %q (present %v), want %q", catalog, present, wantCatalog)
		}
		var catalogArgs []string
		for _, arg := range argv {
			if strings.HasPrefix(arg, "model_catalog_json=") {
				catalogArgs = append(catalogArgs, arg)
			}
		}
		if wantCatalog == "" {
			if len(catalogArgs) != 0 {
				t.Errorf("failed catalog leaked into argv: %q", catalogArgs)
			}
		} else if !reflect.DeepEqual(catalogArgs, []string{"model_catalog_json=" + tomlQuote(wantCatalog)}) {
			t.Errorf("catalog marker disagrees with CLI override: marker %q, args %q", catalog, catalogArgs)
		}
	}
	checkLaunch(path)
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(paths.ConfigDir(), "reject"), "", 0o600)
	checkLaunch("")
}

// TestCodexCatalogArgsSkipsOtherProviders: a non-Databricks provider gets no catalog flag.
func TestCodexCatalogArgsSkipsOtherProviders(t *testing.T) {
	p := dbxRow("qa", "databricks-")
	p.Preset = ""
	if args := codexCatalogArgs(p); args != nil {
		t.Errorf("args = %q, want nil", args)
	}
}
