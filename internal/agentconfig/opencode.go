package agentconfig

import (
	"embed"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/textutil"
)

//go:embed template/opencode.json template/opencode-tui.json
var opencodeOverlayFS embed.FS

var _ = schemas.Register(schemas.Format{
	Name: "opencode-overlay", Owner: "internal/agentconfig", Kind: schemas.Derived,
	Glob:    func() string { return filepath.Join(opencodeOverlayDir(), "*.json") },
	Version: 1, Keep: "re-rendered from the template on every launch; opencode reads it, so no schema field",
})

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
	if tuiBody, err = preserveAppliedTheme(tuiPath, tuiTemplate); err != nil {
		return "", nil, "", nil, err
	}
	return cfgPath, cfgBody, tuiPath, tuiBody, nil
}

// OverlayModels yields the overlay's model/small_model; internal/opencode replaces it with the login-aware pick.
var OverlayModels = router.OpencodeTableModels

// applyOpencodeTableModels overwrites model/small_model from the routing table's opencode sonnet/haiku
// cells; a table load error leaves the template's own hard-coded values (its one fallback use) untouched.
func applyOpencodeTableModels(cfgBody []byte) []byte {
	mainModel, smallModel, ok := OverlayModels()
	if !ok {
		return cfgBody
	}
	var doc map[string]any
	if json.Unmarshal(cfgBody, &doc) != nil {
		return cfgBody
	}
	doc["model"], doc["small_model"] = mainModel, smallModel
	out, ok := encodeOverlayJSON(doc)
	if !ok {
		return cfgBody
	}
	return out
}

// encodeOverlayJSON renders doc as the overlay files' two-space JSON (trailing newline) without HTML escaping.
func encodeOverlayJSON(doc any) ([]byte, bool) {
	out, err := textutil.MarshalIndentNoEscape(doc)
	return append(out, '\n'), err == nil
}

// WriteOpencodeOverlay writes the tracked opencode.json/tui.json templates to state/harness/opencode
// and returns their paths; a theme already applied into the existing tui.json (theme apply) survives.
func WriteOpencodeOverlay() (cfg, tui string, err error) {
	if err := os.MkdirAll(opencodeOverlayDir(), 0o700); err != nil {
		return "", "", err
	}
	// The lock spans the theme read and the tui.json write, so a concurrent writer's theme isn't reverted.
	err = paths.WithFileLock(filepath.Join(opencodeOverlayDir(), "tui.json"), func() error {
		cfgPath, cfgBody, tuiPath, tuiBody, err := OpencodeOverlayRendered()
		if err != nil {
			return err
		}
		if _, err := paths.WriteIfChanged(cfgPath, cfgBody, 0o600); err != nil {
			return err
		}
		cfg, tui = cfgPath, tuiPath
		_, err = paths.WriteIfChanged(tuiPath, tuiBody, 0o600)
		return err
	})
	if err != nil {
		return "", "", err
	}
	return cfg, tui, nil
}

// preserveAppliedTheme keeps a theme `theme apply` already wrote into the on-disk tui.json: it reads
// the existing file's "theme" key and overrides the template's before writing, so a repeated overlay
// write (every opencode launch) never clobbers a theme the owner picked after the template rendered.
func preserveAppliedTheme(path string, template []byte) ([]byte, error) {
	cur, existed, err := ReadExisting(path)
	if err != nil {
		return nil, err // never overwrite a theme the owner picked because the read failed
	}
	if !existed {
		return template, nil
	}
	var curDoc map[string]any
	if json.Unmarshal(cur, &curDoc) != nil {
		return template, nil
	}
	theme, ok := curDoc["theme"].(string)
	if !ok {
		return template, nil
	}
	var next map[string]any
	if json.Unmarshal(template, &next) != nil {
		return template, nil
	}
	if next["theme"] == theme {
		return template, nil // already matches; skip the marshal round-trip (it would HTML-escape the keybinds)
	}
	next["theme"] = theme
	out, ok := encodeOverlayJSON(next)
	if !ok {
		return template, nil
	}
	return out, nil
}
