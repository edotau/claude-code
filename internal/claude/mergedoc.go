package claude

import (
	"encoding/json"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/settings"
)

// collapsedPrefix starts the plan warning that names what Normalize dropped from the merged settings.
const collapsedPrefix = "settings collapsed: "

// normalizeDoc collapses duplicate list entries and hooks in doc, appending one warning naming what it dropped.
func normalizeDoc(doc map[string]any, warnings *[]string) (map[string]any, error) {
	data, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	out, changed, err := settings.Normalize(data)
	if err != nil || len(changed) == 0 {
		return doc, err
	}
	*warnings = append(*warnings, collapsedPrefix+strings.Join(changed, "; "))
	norm := map[string]any{}
	return norm, json.Unmarshal(out, &norm)
}

// putNormalized writes the normalized doc as the plan's launch layer.
func putNormalized(plan *agent.Plan, doc map[string]any) error {
	doc, err := normalizeDoc(doc, &plan.Warnings)
	if err != nil {
		return err
	}
	return putLaunchDoc(plan, "", doc) // inline: a per-launch doc must not rewrite the profile's shared file
}
