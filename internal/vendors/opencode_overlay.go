package vendors

import (
	"bytes"
	"embed"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/edotau/claude-code/internal/paths"
)

//go:embed template/opencode.json template/opencode-tui.json
var opencodeOverlayFS embed.FS

// opencodeOverlayDir is state/harness/opencode: the one place the harness writes for opencode, never
// opencode's own ~/.config/opencode (configure/theme apply write there only for the themes file).
func opencodeOverlayDir() string { return filepath.Join(paths.StateDir(), "opencode") }

// OpencodeOverlayRendered computes the overlay's two files without writing them: cfgBody is always the
// tracked template; tuiBody keeps a theme `theme apply` already wrote at tuiPath, else the template's.
func OpencodeOverlayRendered() (cfgPath string, cfgBody []byte, tuiPath string, tuiBody []byte, err error) {
	dir := opencodeOverlayDir()
	cfgBody, err = opencodeOverlayFS.ReadFile("template/opencode.json")
	if err != nil {
		return "", nil, "", nil, err
	}
	tuiTemplate, err := opencodeOverlayFS.ReadFile("template/opencode-tui.json")
	if err != nil {
		return "", nil, "", nil, err
	}
	cfgPath = filepath.Join(dir, "opencode.json")
	tuiPath = filepath.Join(dir, "tui.json")
	tuiBody = preserveAppliedTheme(tuiPath, tuiTemplate)
	return cfgPath, cfgBody, tuiPath, tuiBody, nil
}

// WriteOpencodeOverlay writes the tracked opencode.json/tui.json templates to state/harness/opencode
// and returns their paths; a theme already applied into the existing tui.json (theme apply) survives.
func WriteOpencodeOverlay() (cfg, tui string, err error) {
	cfgPath, cfgBody, tuiPath, tuiBody, err := OpencodeOverlayRendered()
	if err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		return "", "", err
	}
	if _, err := paths.WriteIfChanged(cfgPath, cfgBody, 0o600); err != nil {
		return "", "", err
	}
	if _, err := paths.WriteIfChanged(tuiPath, tuiBody, 0o600); err != nil {
		return "", "", err
	}
	return cfgPath, tuiPath, nil
}

// preserveAppliedTheme keeps a theme `theme apply` already wrote into the on-disk tui.json: it reads
// the existing file's "theme" key and overrides the template's before writing, so a repeated overlay
// write (every opencode launch) never clobbers a theme the owner picked after the template rendered.
func preserveAppliedTheme(path string, template []byte) []byte {
	cur, err := os.ReadFile(path)
	if err != nil {
		return template
	}
	var curDoc map[string]any
	if json.Unmarshal(cur, &curDoc) != nil {
		return template
	}
	theme, ok := curDoc["theme"].(string)
	if !ok {
		return template
	}
	var next map[string]any
	if json.Unmarshal(template, &next) != nil {
		return template
	}
	if next["theme"] == theme {
		return template // already matches; skip the marshal round-trip (it would HTML-escape the keybinds)
	}
	next["theme"] = theme
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if enc.Encode(next) != nil {
		return template
	}
	return buf.Bytes()
}
