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

	"github.com/edotau/claude-code/internal/agent/agenttest"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/paths"
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

// TestPrepareDefersOpencodeOverlayWrite: PrepareConfigs validates and reports drift without writing the
// overlay files; only Apply (the staged write queue) puts them on disk. opencode.jsonc retirement is
// gone — opencode owns ~/.config/opencode/opencode.jsonc now, the harness never reads or moves it.
func TestPrepareDefersOpencodeOverlayWrite(t *testing.T) {
	home := agenttest.Scratch(t)
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
	home := agenttest.Scratch(t)
	path := filepath.Join(paths.StateDir(), "opencode", "tui.json")
	if _, err := agent.RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &bytes.Buffer{}, Agents: []string{"opencode"}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var cfg map[string]any
	raw, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(raw, &cfg) != nil || cfg["theme"] != "gemini-plum" {
		t.Fatalf("tui.json = %s (err %v), want theme gemini-plum", raw, err)
	}
	agenttest.AssertNotExist(t, "opencode's own ~/.config/opencode/tui.json must not be written", filepath.Join(home, ".config", "opencode", "tui.json"))

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
