package settings

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/edotau/claude-code/internal/providers"
)

//go:embed automode.json
var autoModeJSON []byte

// LaunchSettings is the harness's --settings layer for every Claude launch; databricks adds its route's limits.
func LaunchSettings(databricks bool) (map[string]any, error) {
	var auto map[string]any
	if err := json.Unmarshal(autoModeJSON, &auto); err != nil {
		return nil, fmt.Errorf("embedded automode.json: %w", err)
	}
	doc := map[string]any{"autoMode": auto}
	if env := missingTemplateEnv(Template(), userSettings()...); len(env) > 0 {
		doc["env"] = env
	}
	if databricks {
		doc["remoteControlAtStartup"] = false // Remote Control needs the claude.ai login; Databricks has none
	}
	return doc, nil
}

// userSettings reads the user and cwd project settings files; a key any of them sets is never filled.
func userSettings() [][]byte {
	var out [][]byte
	for _, f := range []string{SettingsFile(), filepath.Join(".claude", "settings.json"),
		filepath.Join(".claude", "settings.local.json")} {
		if b, err := os.ReadFile(f); err == nil { // a missing file sets no key
			out = append(out, b)
		}
	}
	return out
}

// missingTemplateEnv returns the template env keys no settings file sets, never secret-named ones.
func missingTemplateEnv(tmpl []byte, files ...[]byte) map[string]any {
	type doc struct {
		Env map[string]any `json:"env"`
	}
	var t doc
	_ = json.Unmarshal(tmpl, &t)
	set := map[string]bool{}
	for _, f := range files {
		var u doc
		_ = json.Unmarshal(f, &u)
		for k := range u.Env {
			set[k] = true
		}
	}
	out := map[string]any{}
	for k, v := range t.Env {
		if !set[k] && !providers.IsSecretName(k) {
			out[k] = v
		}
	}
	return out
}
