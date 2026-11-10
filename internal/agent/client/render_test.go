package client

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/codex"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// scratch sets CLAUDE_CONFIG_DIR to its own temp dir, a keyed provider as default (so every vendor
// renders instead of skipping), and returns the vendor-config home dir.
func scratch(t *testing.T) string {
	t.Helper()
	configDir := vaulttest.Use(t)
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
	t.Cleanup(agent.StubRouter("http://127.0.0.1:9999"))
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

// assertNotExist reports each path that exists though it must not.
func assertNotExist(t *testing.T, why string, ps ...string) {
	t.Helper()
	for _, p := range ps {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s: %s, stat err = %v", p, why, err)
		}
	}
}

// assertExists reports each path that is missing though it must exist.
func assertExists(t *testing.T, why string, ps ...string) {
	t.Helper()
	for _, p := range ps {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s: %s: %v", p, why, err)
		}
	}
}

func TestRunMalformedCopilotHooksWritesNothing(t *testing.T) {
	home := scratch(t)
	hooksPath := filepath.Join(home, ".copilot", "hooks", "claude-code-harness.json")
	if err := os.MkdirAll(filepath.Dir(hooksPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hooksPath, []byte("{not valid json"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	_, err := RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"codex", "opencode", "gemini", "copilot"}})
	if err == nil || !strings.Contains(err.Error(), "copilot-hooks: existing file:") {
		t.Fatalf("expected malformed Copilot hooks error, got %v\n%s", err, out.String())
	}
	for _, rel := range []string{
		".codex/config.toml", ".codex/hooks.json", ".gemini/antigravity-cli/settings.json", ".copilot/settings.json",
	} {
		assertNotExist(t, "must not be written after a render error", filepath.Join(home, rel))
	}
	// opencode's staged overlay write (ahead of copilot's in DefaultAgents order) must not apply either.
	for _, rel := range []string{"opencode.json", "tui.json"} {
		assertNotExist(t, "opencode overlay must not be written after a later render error", filepath.Join(paths.StateDir(), "opencode", rel))
	}
	if strings.Contains(out.String(), ": wrote ") || strings.Contains(out.String(), ": moved ") {
		t.Errorf("reported unapplied writes:\n%s", out.String())
	}
}

// TestPrepareDefersOpencodeOverlayWrite: PrepareConfigs validates and reports drift without writing the
// overlay files; only Apply (the staged write queue) puts them on disk. opencode.jsonc retirement is
// gone — opencode owns ~/.config/opencode/opencode.jsonc now, the harness never reads or moves it.
func TestPrepareDefersOpencodeOverlayWrite(t *testing.T) {
	home := scratch(t)
	overlayPath := filepath.Join(paths.StateDir(), "opencode", "opencode.json")
	var out bytes.Buffer
	p, drift, err := PrepareConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"opencode"}})
	if err != nil || !drift || p == nil {
		t.Fatalf("Prepare = %v, %v, %v", p, drift, err)
	}
	if _, err := os.Stat(overlayPath); !os.IsNotExist(err) {
		t.Fatalf("Prepare wrote the overlay before Apply: %v", err)
	}
	if err := p.Apply(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(overlayPath); err != nil {
		t.Fatalf("Apply did not write the overlay: %v", err)
	}
	if !strings.Contains(out.String(), "opencode: wrote "+overlayPath) {
		t.Fatalf("expected a wrote line for the overlay, got:\n%s", out.String())
	}
}

func assertCodexManagedBlock(t *testing.T, codexCfg []byte) {
	t.Helper()
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
}

// opencode's overlay is a tracked, owner-authored template, not a per-provider render: just the pinned model/variant.
func assertOpencodeOverlayOnly(t *testing.T, home string) {
	t.Helper()
	ocRaw, err := os.ReadFile(filepath.Join(paths.StateDir(), "opencode", "opencode.json"))
	if err != nil {
		t.Fatalf("read opencode overlay: %v", err)
	}
	var oc map[string]any
	if err := json.Unmarshal(ocRaw, &oc); err != nil {
		t.Fatalf("opencode.json not valid JSON: %v", err)
	}
	if _, ok := oc["model"]; !ok {
		t.Errorf("opencode.json missing a model key: %s", ocRaw)
	}
	assertNotExist(t, "opencode's own ~/.config/opencode/opencode.json must not be written",
		filepath.Join(home, ".config", "opencode", "opencode.json"))
}

func TestRunWritesBannerBlocksAndIsIdempotent(t *testing.T) {
	home := scratch(t)
	var out bytes.Buffer
	opts := agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"codex", "opencode", "gemini", "copilot"}}
	drift, err := RenderConfigs(opts)
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
	assertCodexManagedBlock(t, codexCfg)
	assertOpencodeOverlayOnly(t, home)

	// gemini is report-only (agy has no settings-auth equivalent): it writes nothing, ever.
	assertNotExist(t, "gemini must write no settings.json", filepath.Join(home, ".gemini", "settings.json"))
	if !strings.Contains(out.String(), "gemini: no config render") {
		t.Errorf("expected a gemini report-only line, got:\n%s", out.String())
	}

	// A second run with nothing changed must report no drift and touch no file's content.
	var out2 bytes.Buffer
	opts.Out = &out2
	drift, err = RenderConfigs(opts)
	if err != nil {
		t.Fatalf("second client.RenderConfigs: %v", err)
	}
	if drift {
		t.Fatalf("expected no drift on an idempotent second run, output:\n%s", out2.String())
	}
}

const foreignCodex = "# a hand-written line\nsome_other_key = \"keep-me\"\n\n[projects.\"/x\"]\ntrust_level = \"trusted\"\n"

func writeForeign(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertForeignCodexKept(t *testing.T, got string) {
	t.Helper()
	if !strings.Contains(got, "some_other_key = \"keep-me\"") {
		t.Errorf("foreign codex line was dropped:\n%s", got)
	}
	if !strings.Contains(got, "[projects.\"/x\"]") || !strings.Contains(got, "trust_level = \"trusted\"") {
		t.Errorf("foreign [projects] table was dropped or altered:\n%s", got)
	}
	// The managed banner is inserted before the first pre-existing [table] header, not appended after it.
	bannerIdx := strings.Index(got, "# >>> claude-code:managed:begin >>>")
	tableIdx := strings.Index(got, "[projects.\"/x\"]")
	if bannerIdx < 0 {
		t.Fatalf("managed banner missing:\n%s", got)
	}
	if bannerIdx > tableIdx {
		t.Errorf("managed banner (%d) must precede the pre-existing [projects] table (%d):\n%s", bannerIdx, tableIdx, got)
	}
}

func assertCodexBackupKept(t *testing.T, codexPath string) {
	t.Helper()
	assertNotExist(t, "plaintext backup must not be written", codexPath+".backup")
	backup, found, err := codex.LatestCodexBackup(codexPath)
	if err != nil || !found {
		t.Fatalf("encrypted backup missing: found=%v err=%v", found, err)
	}
	if string(backup.Data) != foreignCodex {
		t.Errorf("encrypted backup did not retain original bytes")
	}
}

func TestForeignKeysSurviveAndBackupIsWritten(t *testing.T) {
	home := scratch(t)
	codexPath := filepath.Join(home, ".codex", "config.toml")
	writeForeign(t, codexPath, foreignCodex)
	// opencode's own config is never touched: foreign content must survive byte-for-byte.
	ocPath := filepath.Join(home, ".config", "opencode", "opencode.json")
	foreignOC := `{"provider": {"myown": {"npm": "whatever"}}, "someForeignKey": true}`
	writeForeign(t, ocPath, foreignOC)

	var out bytes.Buffer
	if _, err := RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"codex", "opencode"}}); err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}

	got, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatal(err)
	}
	assertForeignCodexKept(t, string(got))
	assertCodexBackupKept(t, codexPath)

	ocGot, err := os.ReadFile(ocPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(ocGot) != foreignOC {
		t.Errorf("opencode's own config was touched: got %q, want untouched %q", ocGot, foreignOC)
	}
	assertExists(t, "overlay was not written to state/harness/opencode", filepath.Join(paths.StateDir(), "opencode", "opencode.json"))
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
	if _, err := RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"codex"}}); err != nil {
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

// TestVendorBaselineSeededOnFirstWrite: with no prior baseline, the first write seeds one matching
// exactly what was written — today's behavior, now backed by an on-disk comparison basis for next time.
func TestVendorBaselineSeededOnFirstWrite(t *testing.T) {
	home := scratch(t)
	codexPath := filepath.Join(home, ".codex", "config.toml")
	if _, err := RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &bytes.Buffer{}, Agents: []string{"codex"}}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatal(err)
	}
	base, err := codex.LoadCodexBaseline(codexPath)
	if err != nil {
		t.Fatalf("no baseline seeded: %v", err)
	}
	if !bytes.Equal(base, raw) {
		t.Errorf("seeded baseline = %q, want the written file %q", base, raw)
	}
}

// TestVendorBaselineSeededWhenUnchanged: a file already current but with no baseline (installed before
// baselines existed) gets one, so a later hand edit is still detected.
func TestVendorBaselineSeededWhenUnchanged(t *testing.T) {
	home := scratch(t)
	codexPath := filepath.Join(home, ".codex", "config.toml")
	opts := func() agentconfig.RenderOptions {
		return agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &bytes.Buffer{}, Agents: []string{"codex"}}
	}
	if _, err := RenderConfigs(opts()); err != nil {
		t.Fatal(err)
	}
	if err := vault.Update(func(tx *vault.Tx) error {
		for _, name := range tx.Names(vault.SettingsBackups) {
			if strings.HasPrefix(name, "codex-baseline/") {
				if err := tx.Put(vault.SettingsBackups, name, ""); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := RenderConfigs(opts()); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(codexPath)
	base, err := codex.LoadCodexBaseline(codexPath)
	if err != nil {
		t.Fatalf("unchanged render seeded no baseline: %v", err)
	}
	if !bytes.Equal(base, raw) {
		t.Errorf("seeded baseline = %q, want the current file %q", base, raw)
	}
}

// TestLostLineNonInteractiveOverwritesWithWarning: a hand edit inside the managed banner, re-rendered
// non-interactively (Confirm nil — matches launch/configure/make install), writes anyway and warns.
func TestLostLineNonInteractiveOverwritesWithWarning(t *testing.T) {
	home := scratch(t)
	codexPath := filepath.Join(home, ".codex", "config.toml")
	if _, err := RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &bytes.Buffer{}, Agents: []string{"codex"}}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(raw), `model = "gpt-5"`, `model = "gpt-5-custom"`, 1)
	if edited == string(raw) {
		t.Fatal("tamper anchor not found")
	}
	if err := os.WriteFile(codexPath, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"codex"}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "hand-edited line") || !strings.Contains(out.String(), `model = "gpt-5-custom"`) {
		t.Errorf("expected a lost-line warning naming the edited line, got:\n%s", out.String())
	}
	got, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "gpt-5-custom") {
		t.Errorf("non-interactive must still overwrite the hand edit:\n%s", got)
	}
}

// TestConfirmFalseKeepsFile: an interactive caller's Confirm declining the overwrite leaves the file
// (and its hand edit) untouched.
func TestConfirmFalseKeepsFile(t *testing.T) {
	home := scratch(t)
	codexPath := filepath.Join(home, ".codex", "config.toml")
	if _, err := RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &bytes.Buffer{}, Agents: []string{"codex"}}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(codexPath)
	edited := strings.Replace(string(raw), `model = "gpt-5"`, `model = "gpt-5-custom"`, 1)
	if err := os.WriteFile(codexPath, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	opts := agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"codex"}}
	opts.Confirm = func(path string, lost []string) bool { return false }
	if _, err := RenderConfigs(opts); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "kept") {
		t.Errorf("expected a kept-your-edits note, got:\n%s", out.String())
	}
	got, _ := os.ReadFile(codexPath)
	if !strings.Contains(string(got), "gpt-5-custom") {
		t.Errorf("Confirm=false must keep the hand edit:\n%s", got)
	}
}

// TestConfirmTrueWrites: an interactive caller's Confirm accepting the overwrite writes through,
// same as the non-interactive path, but by explicit choice rather than by default.
func TestConfirmTrueWrites(t *testing.T) {
	home := scratch(t)
	codexPath := filepath.Join(home, ".codex", "config.toml")
	if _, err := RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &bytes.Buffer{}, Agents: []string{"codex"}}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(codexPath)
	edited := strings.Replace(string(raw), `model = "gpt-5"`, `model = "gpt-5-custom"`, 1)
	if err := os.WriteFile(codexPath, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &bytes.Buffer{}, Agents: []string{"codex"}}
	opts.Confirm = func(path string, lost []string) bool { return true }
	if _, err := RenderConfigs(opts); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(codexPath)
	if strings.Contains(string(got), "gpt-5-custom") {
		t.Errorf("Confirm=true must overwrite the hand edit:\n%s", got)
	}
}

// TestCheckListsLostLinesWritesNothing: --check reports the lost line without touching the file.
func TestCheckListsLostLinesWritesNothing(t *testing.T) {
	home := scratch(t)
	codexPath := filepath.Join(home, ".codex", "config.toml")
	if _, err := RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &bytes.Buffer{}, Agents: []string{"codex"}}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(codexPath)
	edited := strings.Replace(string(raw), `model = "gpt-5"`, `model = "gpt-5-custom"`, 1)
	if err := os.WriteFile(codexPath, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &out, Check: true, Agents: []string{"codex"}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `model = "gpt-5-custom"`) {
		t.Errorf("--check must list the lost line, got:\n%s", out.String())
	}
	got, _ := os.ReadFile(codexPath)
	if !strings.Contains(string(got), "gpt-5-custom") {
		t.Errorf("--check must not write:\n%s", got)
	}
}

func revertCodex(t *testing.T, home, codexPath string) string {
	t.Helper()
	if err := RevertConfigs(agentconfig.RenderOptions{Home: home, Agents: []string{"codex"}, Out: &bytes.Buffer{}}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatal(err)
	}
	return string(got)
}

// seedCodexBackups writes current to codexPath and backs up each version in order; returns the newest backup.
func seedCodexBackups(t *testing.T, codexPath, current string, versions ...string) codex.CodexConfigBackup {
	t.Helper()
	writeForeign(t, codexPath, current)
	for _, v := range versions {
		if err := codex.CodexBackup(codexPath, []byte(v), true); err != nil {
			t.Fatal(err)
		}
	}
	newest, found, err := codex.LatestCodexBackup(codexPath)
	if err != nil || !found {
		t.Fatalf("backup missing: %v", err)
	}
	return newest
}

// TestRevertRestoresNewestThenStepsBack: --revert restores the most recent version, consumes it
// (removes the version file), and a second --revert steps one version further back.
func TestRevertRestoresNewestThenStepsBack(t *testing.T) {
	vaulttest.Use(t)
	home := t.TempDir()
	codexPath := filepath.Join(home, ".codex", "config.toml")
	older, newer := "older\n", "newer\n"
	newest := seedCodexBackups(t, codexPath, "current\n", older, newer)

	if got := revertCodex(t, home, codexPath); got != newer {
		t.Fatalf("first revert: got %q, want newest version %q", got, newer)
	}
	if err := vault.View(func(tx *vault.Tx) error {
		if _, found := tx.Get(vault.SettingsBackups, newest.Name); found {
			t.Error("restored snapshot was not consumed")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if got := revertCodex(t, home, codexPath); got != older {
		t.Fatalf("second revert: got %q, want next-older version %q", got, older)
	}
}

// tamperCodexConfig edits the managed block in place and returns the edited text.
func tamperCodexConfig(t *testing.T, path string) string {
	t.Helper()
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
	return edited
}

func TestCheckExitsOnDriftAndWritesNothing(t *testing.T) {
	home := scratch(t)
	var out bytes.Buffer
	if drift, err := RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &out, Check: true, Agents: []string{"codex"}}); err != nil {
		t.Fatalf("Run: %v", err)
	} else if !drift {
		t.Fatal("expected drift=true against an empty home")
	}
	path := filepath.Join(home, ".codex", "config.toml")
	assertNotExist(t, "--check must write nothing", path)

	// Write it for real, then hand-edit the managed block and confirm --check reports drift again.
	if _, err := RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"codex"}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	edited := tamperCodexConfig(t, path)
	var out2 bytes.Buffer
	drift, err := RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &out2, Check: true, Agents: []string{"codex"}})
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
	drift, err := RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &out, DryRun: true, Agents: []string{"codex", "opencode", "gemini"}})
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

// TestOpenCodeTUISeedsSystemThemeAndKeepsUserTheme: the overlay's tui.json lands at state/harness/opencode
// (never opencode's own ~/.config/opencode) with the template's default theme (gemini-plum); a theme
// `theme apply` already wrote into the on-disk file survives a later render untouched and drift-free.
func TestOpenCodeTUISeedsSystemThemeAndKeepsUserTheme(t *testing.T) {
	home := scratch(t)
	path := filepath.Join(paths.StateDir(), "opencode", "tui.json")
	if _, err := RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &bytes.Buffer{}, Agents: []string{"opencode"}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var cfg map[string]any
	raw, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(raw, &cfg) != nil || cfg["theme"] != "gemini-plum" {
		t.Fatalf("tui.json = %s (err %v), want theme gemini-plum", raw, err)
	}
	assertNotExist(t, "opencode's own ~/.config/opencode/tui.json must not be written", filepath.Join(home, ".config", "opencode", "tui.json"))

	// Swap only the theme value in the rendered file (as `theme apply` would), keeping the template's own
	// formatting: preserveAppliedTheme re-renders onto the template, so only a theme-field edit round-trips
	// byte-identical; a reformat or keybind edit would (correctly) show as drift.
	edited := strings.Replace(string(raw), `"gemini-plum"`, `"tokyonight"`, 1)
	if edited == string(raw) {
		t.Fatal("tamper anchor not found")
	}
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	drift, err := RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &bytes.Buffer{}, Check: true, Agents: []string{"opencode"}})
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
	if _, err := RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"codex"}}); err != nil {
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
// worktree while vendor destinations stay $HOME-rooted (agentconfig.RenderOptions.Home left unset, matching production,
// where os.UserHomeDir() resolves the real process HOME). Without --vendors nothing is written under
// that HOME's vendor dirs; with it, writes land exactly as before.
func TestScratchConfigDirSkipsVendorWritesWithoutFlag(t *testing.T) {
	scratch(t) // sets CLAUDE_CONFIG_DIR + a keyed provider overlay; its own returned Home is unused here
	vendorHome := t.TempDir()
	t.Setenv("HOME", vendorHome)
	t.Setenv("CODEX_HOME", filepath.Join(vendorHome, ".codex"))

	var out bytes.Buffer
	drift, err := RenderConfigs(agentconfig.RenderOptions{Out: &out, Agents: []string{"codex", "opencode", "gemini", "copilot"}})
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}
	if drift {
		t.Fatal("a skipped render must report no drift")
	}
	if !strings.Contains(out.String(), "skipping agent configs") {
		t.Fatalf("expected the skip note, got:\n%s", out.String())
	}
	assertNotExist(t, "expected no vendor writes under the scratch HOME",
		filepath.Join(vendorHome, ".codex"), filepath.Join(vendorHome, ".config", "opencode"),
		filepath.Join(paths.StateDir(), "opencode"), filepath.Join(vendorHome, ".copilot"))

	var out2 bytes.Buffer
	if _, err := RenderConfigs(agentconfig.RenderOptions{Out: &out2, ForceHomes: true, Agents: []string{"codex", "opencode", "copilot"}}); err != nil {
		t.Fatalf("Run with --vendors: %v\n%s", err, out2.String())
	}
	assertExists(t, "--vendors must still write the vendor configs",
		filepath.Join(vendorHome, ".codex", "config.toml"), filepath.Join(paths.StateDir(), "opencode", "opencode.json"),
		filepath.Join(vendorHome, ".copilot", "hooks", "claude-code-harness.json"))
}

// TestListAgentsRegistryLoadErrorNoPanic proves the roster survives a providers.Load() failure and no longer
// lists the folded-in "api" agent.
func TestListAgentsRegistryLoadErrorNoPanic(t *testing.T) {
	vaulttest.Use(t)
	t.Setenv(providers.EnvSessionProfile, "")
	vaulttest.Put(t, vault.Overlay, "providers", "{not valid json")
	if _, err := providers.Load(); err == nil {
		t.Fatal("providers.Load() must fail on malformed overlay JSON for this test to prove anything")
	}

	var lines []string
	for _, r := range RosterRows() { // must not panic despite a nil registry from the failed Load()
		lines = append(lines, r.Line())
	}
	out := strings.Join(lines, "\n")
	if !strings.Contains(out, " claude ") {
		t.Errorf("no claude row in output:\n%s", out)
	}
	for _, line := range lines {
		if f := strings.Fields(line); len(f) >= 2 && f[1] == "api" {
			t.Errorf("api agent still listed: %q", line)
		}
	}
}

type capabilitySchemaRow struct {
	Name, Binary string
	Caps         map[string]string
	Events       []string
}

func assertCapabilitySchemaRow(t *testing.T, r capabilitySchemaRow) {
	t.Helper()
	for _, k := range []string{"cost", "background", "live", "remote", "env", "system-prompt"} {
		if r.Caps[k] == "" {
			t.Errorf("%s: caps lacks %s", r.Name, k)
		}
	}
	if r.Binary == "" || !slices.Contains(r.Events, "start") || !slices.Contains(r.Events, "result") {
		t.Errorf("%s: binary %q events %v", r.Name, r.Binary, r.Events)
	}
}

// agents --format json carries each agent's binary, the cost/background/live/remote levels and its event kinds.
func TestAgentsJSONCapabilitySchema(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	out, err := json.Marshal(RosterRows())
	if err != nil {
		t.Fatal(err)
	}
	var rows []capabilitySchemaRow
	if err := json.Unmarshal(out, &rows); err != nil {
		t.Fatal(err, string(out))
	}
	byName := map[string]int{}
	for i, r := range rows {
		byName[r.Name] = i
		assertCapabilitySchemaRow(t, r)
	}
	if c := rows[byName["claude"]]; c.Caps["live"] != "native" || c.Caps["cost"] != "native" {
		t.Errorf("claude caps = %v, want live and cost native", c.Caps)
	}
	if g := rows[byName["gemini"]]; g.Binary != "agy" || g.Caps["live"] != "native" {
		t.Errorf("gemini = %+v, want binary agy and live native", g)
	}
}

// Ties DefaultAgents to the render order, which shows in generate's output.
func TestDefaultAgentsOrder(t *testing.T) {
	if want := []string{"codex", "opencode", "gemini", "copilot"}; !slices.Equal(DefaultAgents, want) {
		t.Errorf("DefaultAgents = %v, want %v", DefaultAgents, want)
	}
}
