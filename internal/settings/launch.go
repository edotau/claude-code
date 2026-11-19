package settings

import (
	_ "embed"
	"encoding/json"
	"fmt"
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
	if databricks {
		doc["remoteControlAtStartup"] = false // Remote Control needs the claude.ai login; Databricks has none
	}
	return doc, nil
}
