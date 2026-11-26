package agentconfig

import (
	"encoding/json"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"

	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/schemas"
	"github.com/edotau/claude-code/internal/platform/textutil"
)

var _ = schemas.Register(schemas.Format{
	Name: "opencode-overlay", Owner: "internal/routing/agentconfig", Kind: schemas.Derived,
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
	if roots := OpencodeSkillRoots(""); len(roots) > 0 {
		doc["skills"] = map[string]any{"paths": roots}
	}
	if mainModel, smallModel, ok := models(); ok {
		doc["model"], doc["small_model"] = mainModel, smallModel
	}
	out, ok := encodeOverlayJSON(doc)
	if !ok {
		return cfgBody
	}
	return out
}

// OpencodeSkillRoots lists each skill under one root (opencode loads a doubled skill racily): global, then project.
func OpencodeSkillRoots(workDir string) []string {
	home, err := os.UserHomeDir()
	if err != nil {
		if u, uerr := user.Current(); uerr == nil {
			home = u.HomeDir
		}
	}
	seen := map[string]bool{}
	roots := skillRootPair(filepath.Join(paths.ConfigDir(), "skills"), filepath.Join(home, ".agents", "skills"), seen)
	for dir := workDir; workDir != "" && dir != home && dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		roots = append(roots, skillRootPair(filepath.Join(dir, ".claude", "skills"), filepath.Join(dir, ".agents", "skills"), seen)...)
	}
	return roots
}

// skillRootPair is own, its hidden subdirs (a scan skips them), and each shared dir entry not yet seen.
func skillRootPair(own, shared string, seen map[string]bool) []string {
	roots := ownSkillRoots(own, seen)
	entries, _ := os.ReadDir(shared)
	for _, e := range entries {
		dir := filepath.Join(shared, e.Name())
		real := skillReal(dir)
		if real == "" || seen["real:"+real] || seen["name:"+e.Name()] {
			continue
		}
		seen["real:"+real], seen["name:"+e.Name()] = true, true
		roots = append(roots, dir)
	}
	return roots
}

// ownSkillRoots is own (when present) and its hidden subdirs, marking every skill in own as seen.
func ownSkillRoots(own string, seen map[string]bool) []string {
	var roots []string
	if fi, err := os.Stat(own); err == nil && fi.IsDir() {
		roots = append(roots, own)
	}
	ownEntries, _ := os.ReadDir(own)
	for _, e := range ownEntries {
		if real := skillReal(filepath.Join(own, e.Name())); real != "" {
			seen["real:"+real], seen["name:"+e.Name()] = true, true
		}
		if e.Name()[0] == '.' && e.IsDir() {
			roots = append(roots, filepath.Join(own, e.Name()))
		}
	}
	return roots
}

// skillReal is dir's resolved path when it is or holds a SKILL.md, else "" (a dangling or empty entry hides nothing).
func skillReal(dir string) string {
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return ""
	}
	found := false
	_ = filepath.WalkDir(real, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Name() == "SKILL.md" {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	if !found {
		return ""
	}
	return real
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
