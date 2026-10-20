package generate

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// scratch sets CLAUDE_CONFIG_DIR to its own temp dir, a keyed provider as default (so every vendor
// renders instead of skipping), and returns the vendor-config home dir.
func scratch(t *testing.T) string {
	t.Helper()
	configDir := t.TempDir()
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("GEMINI_CLI_HOME", filepath.Join(home, ".gemini-cli-home-unused"))
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex-home-unused"))
	// Isolated from the real machine's opencode auth.json/opencode.jsonc (ResolveTarget probes both unconditionally).
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("PATH", t.TempDir()) // no real opencode: a 2.x CLI would report its own logins instead of auth.json
	t.Setenv("HARNESS_PROVIDER", "")
	t.Setenv("GEMINI_API_KEY", "sk-test-gemini")
	t.Setenv("OPENROUTER_API_KEY", "sk-test-openrouter")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	overlay := `{"default": "openai"}`
	t.Setenv("OPENAI_API_KEY", "sk-test-openai")
	if err := os.WriteFile(filepath.Join(mkEnvD(t, configDir), "providers.json"), []byte(overlay), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(configDir, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "CLAUDE.md"), []byte("# CLAUDE.md\n\nfixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestRunWritesBannerBlocksAndIsIdempotent(t *testing.T) {
	home := scratch(t)
	var out bytes.Buffer
	drift, err := Run(Options{Home: home, Out: &out, Agents: []string{"codex", "opencode", "gemini", "copilot"}})
	if err != nil {
		t.Fatalf("Run: %v\noutput:\n%s", err, out.String())
	}
	if !drift {
		t.Fatal("expected drift=true on first write")
	}

	codexCfg, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if err != nil {
		t.Fatalf("read codex config: %v", err)
	}
	if !strings.Contains(string(codexCfg), "[model_providers.claude-code]") {
		t.Errorf("codex config missing managed block:\n%s", codexCfg)
	}
	if !strings.Contains(string(codexCfg), "# >>> claude-code:managed:begin >>>") {
		t.Errorf("codex config missing banner markers:\n%s", codexCfg)
	}

	ocRaw, err := os.ReadFile(filepath.Join(home, ".config", "opencode", "opencode.json"))
	if err != nil {
		t.Fatalf("read opencode config: %v", err)
	}
	var oc map[string]any
	if err := json.Unmarshal(ocRaw, &oc); err != nil {
		t.Fatalf("opencode.json not valid JSON: %v", err)
	}
	if _, ok := oc["provider"].(map[string]any)["harness-openai"]; !ok {
		t.Errorf("opencode.json missing harness-openai provider entry: %s", ocRaw)
	}

	// gemini is report-only (agy has no settings-auth equivalent): it writes nothing, ever.
	if _, err := os.Stat(filepath.Join(home, ".gemini", "settings.json")); !os.IsNotExist(err) {
		t.Fatalf("gemini must write no settings.json, stat err = %v", err)
	}
	if !strings.Contains(out.String(), "gemini: no config render") {
		t.Errorf("expected a gemini report-only line, got:\n%s", out.String())
	}

	// A second run with nothing changed must report no drift and touch no file's content.
	var out2 bytes.Buffer
	drift, err = Run(Options{Home: home, Out: &out2, Agents: []string{"codex", "opencode", "gemini", "copilot"}})
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if drift {
		t.Fatalf("expected no drift on an idempotent second run, output:\n%s", out2.String())
	}
}

func TestForeignKeysSurviveAndBackupIsWritten(t *testing.T) {
	home := scratch(t)
	codexPath := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(codexPath), 0o755); err != nil {
		t.Fatal(err)
	}
	foreign := "# a hand-written line\nsome_other_key = \"keep-me\"\n"
	if err := os.WriteFile(codexPath, []byte(foreign), 0o644); err != nil {
		t.Fatal(err)
	}
	ocPath := filepath.Join(home, ".config", "opencode", "opencode.json")
	if err := os.MkdirAll(filepath.Dir(ocPath), 0o755); err != nil {
		t.Fatal(err)
	}
	foreignOC := `{"provider": {"myown": {"npm": "whatever"}}, "someForeignKey": true}`
	if err := os.WriteFile(ocPath, []byte(foreignOC), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if _, err := Run(Options{Home: home, Out: &out, Agents: []string{"codex", "opencode"}}); err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}

	got, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "some_other_key = \"keep-me\"") {
		t.Errorf("foreign codex line was dropped:\n%s", got)
	}
	if _, err := os.Stat(codexPath + ".backup"); err != nil {
		t.Errorf("expected a .backup file: %v", err)
	}
	backup, _ := os.ReadFile(codexPath + ".backup")
	if string(backup) != foreign {
		t.Errorf("backup = %q, want original content %q", backup, foreign)
	}

	ocGot, err := os.ReadFile(ocPath)
	if err != nil {
		t.Fatal(err)
	}
	var oc map[string]any
	if err := json.Unmarshal(ocGot, &oc); err != nil {
		t.Fatalf("opencode.json not valid JSON: %v", err)
	}
	if _, ok := oc["provider"].(map[string]any)["myown"]; !ok {
		t.Errorf("foreign opencode provider entry was dropped: %s", ocGot)
	}
	if oc["someForeignKey"] != true {
		t.Errorf("foreign top-level opencode key was dropped: %s", ocGot)
	}
}

func TestCheckExitsOnDriftAndWritesNothing(t *testing.T) {
	home := scratch(t)
	var out bytes.Buffer
	if drift, err := Run(Options{Home: home, Out: &out, Check: true, Agents: []string{"codex"}}); err != nil {
		t.Fatalf("Run: %v", err)
	} else if !drift {
		t.Fatal("expected drift=true against an empty home")
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "config.toml")); !os.IsNotExist(err) {
		t.Fatalf("--check must write nothing, stat err = %v", err)
	}

	// Write it for real, then hand-edit the managed block and confirm --check reports drift again.
	if _, err := Run(Options{Home: home, Out: &out, Agents: []string{"codex"}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	path := filepath.Join(home, ".codex", "config.toml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(raw), "model_provider", "model_provider_TAMPERED", 1)
	if edited == string(raw) {
		t.Fatal("tamper replace found no anchor")
	}
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	var out2 bytes.Buffer
	drift, err := Run(Options{Home: home, Out: &out2, Check: true, Agents: []string{"codex"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !drift {
		t.Fatal("expected drift=true after a hand edit")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != edited {
		t.Fatal("--check must not write")
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	home := scratch(t)
	var out bytes.Buffer
	drift, err := Run(Options{Home: home, Out: &out, DryRun: true, Agents: []string{"codex", "opencode", "gemini"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !drift {
		t.Fatal("expected drift=true (would write) on an empty home")
	}
	if _, err := os.Stat(filepath.Join(home, ".codex")); !os.IsNotExist(err) {
		t.Fatalf("--dry-run must write nothing, stat err = %v", err)
	}
	if out.Len() == 0 {
		t.Fatal("expected --dry-run to print the rendered blocks")
	}
}

func TestOpenCodeTUISeedsSystemThemeAndKeepsUserTheme(t *testing.T) {
	home := scratch(t)
	path := filepath.Join(home, ".config", "opencode", "tui.json")
	if _, err := Run(Options{Home: home, Out: &bytes.Buffer{}, Agents: []string{"opencode"}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var cfg map[string]any
	raw, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(raw, &cfg) != nil || cfg["theme"] != "system" {
		t.Fatalf("tui.json = %s (err %v), want theme system", raw, err)
	}

	if err := os.WriteFile(path, []byte(`{"theme":"tokyonight","keybinds":{"leader":"ctrl+x"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	drift, err := Run(Options{Home: home, Out: &bytes.Buffer{}, Check: true, Agents: []string{"opencode"}})
	if err != nil {
		t.Fatalf("Run --check: %v", err)
	}
	if got, _ := os.ReadFile(path); !strings.Contains(string(got), "tokyonight") {
		t.Fatalf("user theme overwritten: %s", got)
	}
	if drift {
		t.Fatal("a user-chosen theme must not count as drift")
	}
}

func TestCodexOnOwnLoginSkipsInsteadOfFailing(t *testing.T) {
	home := scratch(t)
	overlay := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "env.d", "providers.json")
	if err := os.WriteFile(overlay, []byte(`{"default": "chatgpt"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	codexHome := os.Getenv("CODEX_HOME")
	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), []byte(`{"tokens":{"id_token":"x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := Run(Options{Home: home, Out: &out, Agents: []string{"codex"}}); err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "codex: skipped") {
		t.Fatalf("want a codex skip line, got:\n%s", out.String())
	}
	// Hook trust entries land regardless of login type; only the model_providers block is skipped.
	raw, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if err != nil {
		t.Fatalf("read config.toml: %v", err)
	}
	if strings.Contains(string(raw), "model_providers.claude-code") {
		t.Fatalf("own-login codex must not get a model_providers block:\n%s", raw)
	}
	if !strings.Contains(string(raw), "[hooks.state.") {
		t.Fatalf("own-login codex should still get hook trust entries:\n%s", raw)
	}
}
