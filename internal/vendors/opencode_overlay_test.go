package vendors

import (
	"encoding/json"
	"os"
	"testing"
)

// TestOpencodeOverlayTemplatesParse pins the embedded templates as valid JSON.
func TestOpencodeOverlayTemplatesParse(t *testing.T) {
	for _, name := range []string{"template/opencode.json", "template/opencode-tui.json"} {
		body, err := opencodeOverlayFS.ReadFile(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var doc map[string]any
		if err := json.Unmarshal(body, &doc); err != nil {
			t.Fatalf("%s: not valid JSON: %v", name, err)
		}
	}
}

// TestWriteOpencodeOverlayWritesBothFiles checks the two overlay files land at state/harness/opencode
// with mode 0600, and that the cfg file's content matches the embedded template verbatim.
func TestWriteOpencodeOverlayWritesBothFiles(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	cfg, tui, err := WriteOpencodeOverlay()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{cfg, tui} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, want 0600", p, fi.Mode().Perm())
		}
	}
	want, err := opencodeOverlayFS.ReadFile("template/opencode.json")
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("opencode.json = %s, want the embedded template verbatim", got)
	}
}

// TestLaunchEnvCarriesOverlayPaths checks every opencode launch (applyLaunch) carries both overlay env vars.
func TestLaunchEnvCarriesOverlayPaths(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	env, _ := applyLaunch("opencode", nil, nil, true)
	var haveCfg, haveTUI bool
	for _, kv := range env {
		switch {
		case len(kv) > len("OPENCODE_CONFIG=") && kv[:len("OPENCODE_CONFIG=")] == "OPENCODE_CONFIG=":
			haveCfg = true
		case len(kv) > len("OPENCODE_TUI_CONFIG=") && kv[:len("OPENCODE_TUI_CONFIG=")] == "OPENCODE_TUI_CONFIG=":
			haveTUI = true
		}
	}
	if !haveCfg || !haveTUI {
		t.Errorf("applyLaunch(opencode) env = %v, want both OPENCODE_CONFIG and OPENCODE_TUI_CONFIG", env)
	}
}
