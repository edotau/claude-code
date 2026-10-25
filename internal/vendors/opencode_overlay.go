package vendors

import (
	"bytes"
	"embed"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/routetable"
)

//go:embed template/opencode.json template/opencode-tui.json
var opencodeOverlayFS embed.FS

// opencodeOverlayDir is state/harness/opencode: the one place the harness writes for opencode, never
// opencode's own ~/.config/opencode (configure/theme apply write there only for the themes file).
func opencodeOverlayDir() string { return filepath.Join(paths.StateDir(), "opencode") }

// OpencodeOverlayRendered computes the overlay's two files without writing them: cfgBody is the tracked
// template with model/small_model overwritten from the routing table's opencode sonnet/haiku cells (the
// template's own values are only a fallback for a table load error); tuiBody keeps a theme `theme apply`
// already wrote at tuiPath, else the template's.
func OpencodeOverlayRendered() (cfgPath string, cfgBody []byte, tuiPath string, tuiBody []byte, err error) {
	dir := opencodeOverlayDir()
	cfgBody, err = opencodeOverlayFS.ReadFile("template/opencode.json")
	if err != nil {
		return "", nil, "", nil, err
	}
	cfgBody = applyOpencodeTableModels(cfgBody)
	tuiTemplate, err := opencodeOverlayFS.ReadFile("template/opencode-tui.json")
	if err != nil {
		return "", nil, "", nil, err
	}
	cfgPath = filepath.Join(dir, "opencode.json")
	tuiPath = filepath.Join(dir, "tui.json")
	tuiBody = preserveAppliedTheme(tuiPath, tuiTemplate)
	return cfgPath, cfgBody, tuiPath, tuiBody, nil
}

// applyOpencodeTableModels overwrites model/small_model from the routing table's opencode sonnet/haiku
// cells; a table load error leaves the template's own hard-coded values (its one fallback use) untouched.
func applyOpencodeTableModels(cfgBody []byte) []byte {
	t, err := routetable.Load()
	if err != nil {
		return cfgBody
	}
	sonnet, ok := t.Cell("opencode", "sonnet")
	if !ok || sonnet.Model == "" {
		return cfgBody
	}
	haiku, ok := t.Cell("opencode", "haiku")
	if !ok || haiku.Model == "" {
		return cfgBody
	}
	var doc map[string]any
	if json.Unmarshal(cfgBody, &doc) != nil {
		return cfgBody
	}
	doc["model"], doc["small_model"] = sonnet.Model, haiku.Model
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if enc.Encode(doc) != nil {
		return cfgBody
	}
	return buf.Bytes()
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
