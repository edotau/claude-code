package settings

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"

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
	if env := missingTemplateEnv(Template(), userSettings()); len(env) > 0 {
		doc["env"] = env
	}
	if databricks {
		doc["remoteControlAtStartup"] = false // Remote Control needs the claude.ai login; Databricks has none
	}
	return doc, nil
}

func userSettings() []byte {
	b, _ := os.ReadFile(SettingsFile()) // a missing file means every template key is missing
	return b
}

// missingTemplateEnv returns the template env keys absent from the user's settings.json env, never secret-named ones.
func missingTemplateEnv(tmpl, user []byte) map[string]any {
	var t, u struct {
		Env map[string]any `json:"env"`
	}
	_ = json.Unmarshal(tmpl, &t)
	_ = json.Unmarshal(user, &u)
	out := map[string]any{}
	for k, v := range t.Env {
		if _, set := u.Env[k]; !set && !providers.IsSecretName(k) {
			out[k] = v
		}
	}
	return out
}
