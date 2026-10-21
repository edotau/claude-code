package generate

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestMoveBackupPreservesCollision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.jsonc")
	base := path + ".bak-20261020T120000.123456789Z"
	original := []byte("original backup\n")
	data := []byte("new backup\n")
	for name, content := range map[string][]byte{base: original, path: data} {
		if err := os.WriteFile(name, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	backup, err := moveBackup(path, base)
	if err != nil {
		t.Fatal(err)
	}
	if backup != base+"-1" {
		t.Fatalf("backup = %q, want %q", backup, base+"-1")
	}
	for name, want := range map[string][]byte{base: original, backup: data} {
		got, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("source still exists or cannot be checked after move: %v", err)
	}
}

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
	vaulttest.Put(t, vault.Overlay, "providers", overlay)
	if err := os.MkdirAll(filepath.Join(configDir, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "CLAUDE.md"), []byte("# CLAUDE.md\n\nfixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestRunMalformedCopilotHooksWritesNothing(t *testing.T) {
	home := scratch(t)
	hooksPath := filepath.Join(home, ".copilot", "hooks", "claude-code-harness.json")
	jsoncPath := filepath.Join(home, ".config", "opencode", "opencode.jsonc")
	for path, body := range map[string]string{hooksPath: "{not valid json", jsoncPath: "{}\n"} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	_, err := Run(Options{Home: home, Vendors: true, Out: &out, Agents: []string{"codex", "opencode", "gemini", "copilot"}})
	if err == nil || !strings.Contains(err.Error(), "copilot-hooks: existing file:") {
		t.Fatalf("expected malformed Copilot hooks error, got %v\n%s", err, out.String())
	}
	for _, rel := range []string{
		".codex/config.toml", ".codex/hooks.json", ".config/opencode/opencode.json",
		".config/opencode/tui.json", ".gemini/antigravity-cli/settings.json", ".copilot/settings.json",
	} {
		if _, err := os.Stat(filepath.Join(home, rel)); !os.IsNotExist(err) {
			t.Errorf("%s must not be written after a render error: %v", rel, err)
		}
	}
	if raw, err := os.ReadFile(jsoncPath); err != nil || string(raw) != "{}\n" {
		t.Errorf("JSONC must not be retired after a render error: %q, %v", raw, err)
	}
	if strings.Contains(out.String(), ": wrote ") || strings.Contains(out.String(), ": moved ") {
		t.Errorf("reported unapplied writes:\n%s", out.String())
	}
}

func TestPrepareDefersWritesAndJSONCRetirement(t *testing.T) {
	home := scratch(t)
	jsoncPath := filepath.Join(home, ".config", "opencode", "opencode.jsonc")
	if err := os.MkdirAll(filepath.Dir(jsoncPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jsoncPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	p, drift, err := Prepare(Options{Home: home, Vendors: true, Out: &out, Agents: []string{"opencode"}})
	if err != nil || !drift || p == nil {
		t.Fatalf("Prepare = %v, %v, %v", p, drift, err)
	}
	if _, err := os.Stat(opencodeJSONPath(home)); !os.IsNotExist(err) {
		t.Fatalf("Prepare wrote opencode.json: %v", err)
	}
	if _, err := os.Stat(jsoncPath); err != nil {
		t.Fatalf("Prepare retired opencode.jsonc: %v", err)
	}
	if err := p.Apply(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(opencodeJSONPath(home)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(jsoncPath); !os.IsNotExist(err) {
		t.Fatalf("Apply did not retire opencode.jsonc: %v", err)
	}
	backups, err := filepath.Glob(jsoncPath + ".bak-*")
	if err != nil || len(backups) != 1 {
		t.Fatalf("JSONC backups = %v, %v", backups, err)
	}
	stamp := strings.TrimPrefix(backups[0], jsoncPath+".bak-")
	if _, err := time.Parse("20060102T150405.000000000Z", stamp); err != nil {
		t.Fatalf("backup timestamp lacks nanosecond precision: %q: %v", stamp, err)
	}
	moved := strings.Index(out.String(), "opencode: moved ")
	wrote := strings.Index(out.String(), "opencode: wrote ")
	if moved < 0 || wrote < moved {
		t.Fatalf("JSONC must be retired before writing opencode.json:\n%s", out.String())
	}
}

func TestRunWritesBannerBlocksAndIsIdempotent(t *testing.T) {
	home := scratch(t)
	var out bytes.Buffer
	drift, err := Run(Options{Home: home, Vendors: true, Out: &out, Agents: []string{"codex", "opencode", "gemini", "copilot"}})
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
	// Top-level approval/sandbox keys from vendorconfig must precede any [table] header.
	approvalIdx := strings.Index(string(codexCfg), "approval_policy")
	sandboxIdx := strings.Index(string(codexCfg), "sandbox_mode")
	firstTable := strings.Index(string(codexCfg), "[")
	if approvalIdx < 0 || sandboxIdx < 0 {
		t.Fatalf("codex config missing approval_policy/sandbox_mode:\n%s", codexCfg)
	}
	if firstTable >= 0 && (approvalIdx > firstTable || sandboxIdx > firstTable) {
		t.Errorf("approval_policy/sandbox_mode must precede the first [table] header:\n%s", codexCfg)
	}
	if n := strings.Count(string(codexCfg), `cli_auth_credentials_store = "keyring"`); n != 1 {
		t.Errorf("cli_auth_credentials_store occurs %d times, want 1:\n%s", n, codexCfg)
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
	drift, err = Run(Options{Home: home, Vendors: true, Out: &out2, Agents: []string{"codex", "opencode", "gemini", "copilot"}})
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
	foreign := "# a hand-written line\nsome_other_key = \"keep-me\"\n\n[projects.\"/x\"]\ntrust_level = \"trusted\"\n"
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
	if _, err := Run(Options{Home: home, Vendors: true, Out: &out, Agents: []string{"codex", "opencode"}}); err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}

	got, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "some_other_key = \"keep-me\"") {
		t.Errorf("foreign codex line was dropped:\n%s", got)
	}
	if !strings.Contains(string(got), "[projects.\"/x\"]") || !strings.Contains(string(got), "trust_level = \"trusted\"") {
		t.Errorf("foreign [projects] table was dropped or altered:\n%s", got)
	}
	// The managed banner is inserted before the first pre-existing [table] header, not appended after it.
	bannerIdx := strings.Index(string(got), "# >>> claude-code:managed:begin >>>")
	tableIdx := strings.Index(string(got), "[projects.\"/x\"]")
	if bannerIdx < 0 {
		t.Fatalf("managed banner missing:\n%s", got)
	}
	if bannerIdx > tableIdx {
		t.Errorf("managed banner (%d) must precede the pre-existing [projects] table (%d):\n%s", bannerIdx, tableIdx, got)
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

// A user-set cli_auth_credentials_store outside the managed banner is kept verbatim, and the render
// reports it rather than silently overwriting a value the user chose for themselves.
func TestCodexCredStoreKeyUserValueKept(t *testing.T) {
	home := scratch(t)
	codexPath := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(codexPath), 0o755); err != nil {
		t.Fatal(err)
	}
	foreign := "cli_auth_credentials_store = \"file\"\n"
	if err := os.WriteFile(codexPath, []byte(foreign), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := Run(Options{Home: home, Vendors: true, Out: &out, Agents: []string{"codex"}}); err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}
	got, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `cli_auth_credentials_store = "file"`) {
		t.Errorf("user's cli_auth_credentials_store was overwritten:\n%s", got)
	}
	if strings.Contains(string(got), `cli_auth_credentials_store = "keyring"`) {
		t.Errorf("harness must not add its own key alongside the user's:\n%s", got)
	}
	if !strings.Contains(out.String(), "codex: cli_auth_credentials_store already set by the user; leaving it") {
		t.Errorf("expected the leaving-it note, got:\n%s", out.String())
	}
}

func TestCheckExitsOnDriftAndWritesNothing(t *testing.T) {
	home := scratch(t)
	var out bytes.Buffer
	if drift, err := Run(Options{Home: home, Vendors: true, Out: &out, Check: true, Agents: []string{"codex"}}); err != nil {
		t.Fatalf("Run: %v", err)
	} else if !drift {
		t.Fatal("expected drift=true against an empty home")
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "config.toml")); !os.IsNotExist(err) {
		t.Fatalf("--check must write nothing, stat err = %v", err)
	}

	// Write it for real, then hand-edit the managed block and confirm --check reports drift again.
	if _, err := Run(Options{Home: home, Vendors: true, Out: &out, Agents: []string{"codex"}}); err != nil {
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
	drift, err := Run(Options{Home: home, Vendors: true, Out: &out2, Check: true, Agents: []string{"codex"}})
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
	drift, err := Run(Options{Home: home, Vendors: true, Out: &out, DryRun: true, Agents: []string{"codex", "opencode", "gemini"}})
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
	if _, err := Run(Options{Home: home, Vendors: true, Out: &bytes.Buffer{}, Agents: []string{"opencode"}}); err != nil {
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
	drift, err := Run(Options{Home: home, Vendors: true, Out: &bytes.Buffer{}, Check: true, Agents: []string{"opencode"}})
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
	vaulttest.Put(t, vault.Overlay, "providers", `{"default": "chatgpt"}`)
	codexHome := os.Getenv("CODEX_HOME")
	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), []byte(`{"tokens":{"id_token":"x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := Run(Options{Home: home, Vendors: true, Out: &out, Agents: []string{"codex"}}); err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}
	// Own login no longer fully "skips": the model_providers block is skipped, but approval/sandbox
	// keys (vendorconfig, independent of login type) still render.
	if !strings.Contains(out.String(), "codex: chatgpt runs on codex's own login; writing approval/sandbox keys only") {
		t.Fatalf("want the own-login codex report line, got:\n%s", out.String())
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
	// Top-level approval/sandbox keys are vendorconfig's, independent of login type: written even here.
	if !strings.Contains(string(raw), "approval_policy") || !strings.Contains(string(raw), "sandbox_mode") {
		t.Fatalf("own-login codex should still get approval_policy/sandbox_mode:\n%s", raw)
	}
}

// TestScratchConfigDirSkipsVendorWritesWithoutFlag reproduces the P0: CLAUDE_CONFIG_DIR pointed at a
// worktree while vendor destinations stay $HOME-rooted (Options.Home left unset, matching production,
// where os.UserHomeDir() resolves the real process HOME). Without --vendors nothing is written under
// that HOME's vendor dirs; with it, writes land exactly as before.
func TestScratchConfigDirSkipsVendorWritesWithoutFlag(t *testing.T) {
	scratch(t) // sets CLAUDE_CONFIG_DIR + a keyed provider overlay; its own returned Home is unused here
	vendorHome := t.TempDir()
	t.Setenv("HOME", vendorHome)

	var out bytes.Buffer
	drift, err := Run(Options{Out: &out, Agents: []string{"codex", "opencode", "gemini", "copilot"}})
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}
	if drift {
		t.Fatal("a skipped render must report no drift")
	}
	if !strings.Contains(out.String(), "skipping vendor configs") {
		t.Fatalf("expected the skip note, got:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(vendorHome, ".codex")); !os.IsNotExist(err) {
		t.Fatalf("expected no .codex under the scratch HOME, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(vendorHome, ".config", "opencode")); !os.IsNotExist(err) {
		t.Fatalf("expected no opencode config under the scratch HOME, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(vendorHome, ".copilot")); !os.IsNotExist(err) {
		t.Fatalf("expected no .copilot under the scratch HOME, stat err = %v", err)
	}

	var out2 bytes.Buffer
	if _, err := Run(Options{Out: &out2, Vendors: true, Agents: []string{"codex", "opencode", "copilot"}}); err != nil {
		t.Fatalf("Run with --vendors: %v\n%s", err, out2.String())
	}
	if _, err := os.Stat(filepath.Join(vendorHome, ".codex", "config.toml")); err != nil {
		t.Fatalf("--vendors must still write codex config: %v", err)
	}
	if _, err := os.Stat(filepath.Join(vendorHome, ".config", "opencode", "opencode.json")); err != nil {
		t.Fatalf("--vendors must still write opencode config: %v", err)
	}
	if _, err := os.Stat(filepath.Join(vendorHome, ".copilot", "hooks", "claude-code-harness.json")); err != nil {
		t.Fatalf("--vendors must still write copilot hooks: %v", err)
	}
}
