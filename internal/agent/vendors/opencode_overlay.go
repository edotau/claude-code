package vendors

import (
	"bytes"
	"embed"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
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
	mainModel, smallModel, ok := opencodeOverlayModels()
	if !ok {
		return cfgBody
	}
	var doc map[string]any
	if json.Unmarshal(cfgBody, &doc) != nil {
		return cfgBody
	}
	doc["model"], doc["small_model"] = mainModel, smallModel
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if enc.Encode(doc) != nil {
		return cfgBody
	}
	return buf.Bytes()
}

// OpencodeTableModels returns the routing table's opencode sonnet/haiku models; router sets it (it imports this package).
var OpencodeTableModels func() (mainModel, smallModel string, ok bool)

// opencodeOverlayModels is the overlay's model/small_model: github-copilot/ ids on the copilot-only leg (no
// opencode-go login, so the table's opencode-go cells would be unservable), else the table's opencode cells.
func opencodeOverlayModels() (mainModel, smallModel string, ok bool) {
	if !OpencodeOwnLoginPresent() && OpencodeCopilotLoginPresent() {
		reg, _ := providers.Load()
		if m, s, found := OpencodeCopilotModelIDs(reg); found {
			return "github-copilot/" + m, "github-copilot/" + s, true
		}
	}
	if OpencodeTableModels == nil {
		return "", "", false
	}
	return OpencodeTableModels()
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
