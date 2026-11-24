package agentconfig

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/textutil"
)

var _ = schemas.Register(schemas.Format{
	Name: "opencode-overlay", Owner: "internal/agentconfig", Kind: schemas.Derived,
	Glob:    func() string { return filepath.Join(opencodeOverlayDir(), "*.json") },
	Version: 1, Keep: "re-rendered from the template on every launch; opencode reads it, so no schema field",
})

// opencodeOverlayDir is state/harness/opencode: the only place the harness writes for opencode.
func opencodeOverlayDir() string { return filepath.Join(paths.StateDir(), "opencode") }

// OpencodeOverlayRendered computes the overlay's two files unwritten: table models in, the applied theme kept.
func OpencodeOverlayRendered(models func() (string, string, bool)) (cfgPath string, cfgBody []byte, tuiPath string, tuiBody []byte, err error) {
	dir := opencodeOverlayDir()
	cfgBody = assets.OpencodeConfigTemplate()
	cfgBody = applyOpencodeTableModels(cfgBody, models)
	tuiTemplate := assets.OpencodeTUITemplate()
	cfgPath = filepath.Join(dir, "opencode.json")
	tuiPath = filepath.Join(dir, "tui.json")
	if tuiBody, err = preserveAppliedTheme(tuiPath, tuiTemplate); err != nil {
		return "", nil, "", nil, err
	}
	return cfgPath, cfgBody, tuiPath, tuiBody, nil
}

// applyOpencodeTableModels sets model/small_model from the routing table and skills.paths.
func applyOpencodeTableModels(cfgBody []byte, models func() (string, string, bool)) []byte {
	var doc map[string]any
	if json.Unmarshal(cfgBody, &doc) != nil {
		return cfgBody
	}
	doc["skills"] = map[string]any{"paths": opencodeSkillRoots()}
	if mainModel, smallModel, ok := models(); ok {
		doc["model"], doc["small_model"] = mainModel, smallModel
	}
	out, ok := encodeOverlayJSON(doc)
	if !ok {
		return cfgBody
	}
	return out
}

// opencodeSkillRoots lists each skill under one root: <config>/skills, then ~/.agents/skills entries it lacks.
func opencodeSkillRoots() []string {
	own := filepath.Join(paths.ConfigDir(), "skills")
	roots := []string{own}
	home, err := os.UserHomeDir()
	if err != nil {
		return roots
	}
	shared := filepath.Join(home, ".agents", "skills")
	entries, _ := os.ReadDir(shared)
	for _, e := range entries {
		dir := filepath.Join(shared, e.Name())
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() || e.Name()[0] == '.' {
			continue
		}
		if _, err := os.Lstat(filepath.Join(own, e.Name())); err != nil {
			roots = append(roots, dir)
		}
	}
	return roots
}

// encodeOverlayJSON renders doc as the overlay files' two-space JSON (trailing newline) without HTML escaping.
func encodeOverlayJSON(doc any) ([]byte, bool) {
	out, err := textutil.MarshalIndentNoEscape(doc)
	return append(out, '\n'), err == nil
}

// WriteOpencodeOverlay writes the opencode.json/tui.json overlay and returns their paths; an applied theme survives.
func WriteOpencodeOverlay(models func() (string, string, bool)) (cfg, tui string, err error) {
	if err := os.MkdirAll(opencodeOverlayDir(), 0o700); err != nil {
		return "", "", err
	}
	// The lock spans the theme read and the tui.json write, so a concurrent writer's theme isn't reverted.
	err = paths.WithFileLock(filepath.Join(opencodeOverlayDir(), "tui.json"), func() error {
		cfgPath, cfgBody, tuiPath, tuiBody, err := OpencodeOverlayRendered(models)
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

// preserveAppliedTheme overrides the template's "theme" with the on-disk tui.json's, so a relaunch keeps the pick.
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
