package claude

import (
	"encoding/json"

	"github.com/edotau/claude-code/internal/settings"
)

// normalizeDoc collapses duplicate list entries and hooks in doc; changed names what it dropped.
func normalizeDoc(doc map[string]any) (map[string]any, []string, error) {
	data, err := json.Marshal(doc)
	if err != nil {
		return nil, nil, err
	}
	out, changed, err := settings.Normalize(data)
	if err != nil || len(changed) == 0 {
		return doc, nil, err
	}
	norm := map[string]any{}
	return norm, changed, json.Unmarshal(out, &norm)
}
