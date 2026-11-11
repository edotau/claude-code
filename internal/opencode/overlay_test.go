package opencode

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// TestRenderOpenCodeOverlayReturnsReadError: an unreadable overlay file is an error, not "absent" (silent drift).
func TestRenderOpenCodeOverlayReturnsReadError(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := os.MkdirAll(filepath.Join(paths.StateDir(), "opencode", "opencode.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	changed, err := renderOpenCodeOverlay(agentconfig.RenderOptions{Check: true}, io.Discard)
	if err == nil {
		t.Fatalf("a directory in place of opencode.json read as absent: changed=%v, want an error", changed)
	}
}

func TestOpencodeRulesOnUsesPortableCoreWithoutClaudeMD(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	std := filepath.Join(dir, "rules", "standards")
	if err := os.MkdirAll(std, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		filepath.Join(dir, "CLAUDE.md"): "# c\n",
		filepath.Join(std, "b.md"):      "# b\n",
		filepath.Join(std, "a.md"):      "# a\n",
		filepath.Join(std, "scoped.md"): "---\npaths: [\"*.go\"]\n---\n# s\n",
	}
	for p, body := range files {
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := map[string]any{}
	opencodeRulesOn(cfg)
	want := []string{filepath.Join(std, "a.md"), filepath.Join(std, "b.md")}
	if got := cfg["instructions"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("instructions = %v, want %v", got, want)
	}
}

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

// TestPrepareDefersOpencodeOverlayWrite: PrepareConfigs validates and reports drift without writing the
// overlay files; only Apply (the staged write queue) puts them on disk. opencode.jsonc retirement is
// gone — opencode owns ~/.config/opencode/opencode.jsonc now, the harness never reads or moves it.
func TestPrepareDefersOpencodeOverlayWrite(t *testing.T) {
	home := scratch(t)
	overlayPath := filepath.Join(paths.StateDir(), "opencode", "opencode.json")
	var out bytes.Buffer
	p, drift, err := agent.PrepareConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"opencode"}})
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

// TestOpenCodeTUISeedsSystemThemeAndKeepsUserTheme: the overlay's tui.json lands at state/harness/opencode
// (never opencode's own ~/.config/opencode) with the template's default theme (gemini-plum); a theme
// `theme apply` already wrote into the on-disk file survives a later render untouched and drift-free.
func TestOpenCodeTUISeedsSystemThemeAndKeepsUserTheme(t *testing.T) {
	home := scratch(t)
	path := filepath.Join(paths.StateDir(), "opencode", "tui.json")
	if _, err := agent.RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &bytes.Buffer{}, Agents: []string{"opencode"}}); err != nil {
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
	drift, err := agent.RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &bytes.Buffer{}, Check: true, Agents: []string{"opencode"}})
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
