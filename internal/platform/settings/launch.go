package settings

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/routing/providers"
)

// LaunchSettings is the harness's --settings layer for every Claude launch; databricks adds its route's limits.
func LaunchSettings(databricks bool) (map[string]any, error) {
	var auto map[string]any
	if err := json.Unmarshal(assets.SettingsAutoMode(), &auto); err != nil {
		return nil, fmt.Errorf("embedded automode.json: %w", err)
	}
	doc := map[string]any{"autoMode": auto}
	maps.Copy(doc, missingTemplateKeys(Template(), userSettings()...))
	if databricks {
		doc["remoteControlAtStartup"] = false // Remote Control needs the claude.ai login; Databricks has none
		doc["disableRemoteControl"] = true
	}
	doc["hooks"] = launchHooks()
	return doc, nil
}

// launchGroups are the AgentOnly groups every Claude launch carries: Tavily web answers, the instruction-file gate.
var launchGroups = []string{"web", "safety-claude-edit"}

// launchHooks is each launch group's entries, by event.
func launchHooks() map[string]any {
	out := map[string]any{}
	for _, g := range Registry {
		if !slices.Contains(launchGroups, g.Name) {
			continue
		}
		for _, s := range g.Events {
			entry := map[string]any{"matcher": s.Matcher, "hooks": []any{map[string]any{
				"type": "command", "timeout": s.Seconds(), "command": s.Command()}}}
			prev, _ := out[s.Event].([]any)
			out[s.Event] = append(prev, entry)
		}
	}
	return out
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

// missingTemplateKeys returns the template's keys no settings file sets; env fills key by key, never secret-named ones.
func missingTemplateKeys(tmpl []byte, files ...[]byte) map[string]any {
	var t map[string]any
	_ = json.Unmarshal(tmpl, &t) // the embedded template always parses; a bad one fills nothing
	env, _ := t["env"].(map[string]any)
	for _, f := range files {
		var u map[string]any
		_ = json.Unmarshal(f, &u)
		userEnv, _ := u["env"].(map[string]any)
		for k := range userEnv {
			delete(env, k)
		}
		for k := range u {
			delete(t, k)
		}
	}
	maps.DeleteFunc(env, func(k string, _ any) bool { return providers.IsSecretName(k) })
	delete(t, "$schema")
	delete(t, "env")
	if len(env) > 0 {
		t["env"] = env
	}
	return t
}
