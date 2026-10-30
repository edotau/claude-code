package vendors

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/edotau/claude-code/internal/providers"
)

func buildCodexCatalog(p *providers.Provider, menu []providers.OpencodeModel, raw []byte) ([]byte, string, error) {
	var cat struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(raw, &cat); err != nil {
		return nil, "", err
	}
	if len(cat.Models) == 0 {
		return nil, "", errors.New("no models")
	}
	bySlug := make(map[string]map[string]any)
	byKey := make(map[string]map[string]any)
	for _, m := range cat.Models {
		slug, ok := m["slug"].(string)
		if !ok || slug == "" {
			return nil, "", errors.New("model has no string slug")
		}
		bySlug[slug], byKey[codexModelKey(slug)] = m, m
	}
	var out []map[string]any
	var nativeSlugs []string
	var missing string
	labels := make(map[string]int)
	listed := make(map[string]bool)
	for _, m := range menu {
		if !m.Responses {
			continue
		}
		base := bySlug[codexSlug(m.ID)]
		if base == nil {
			base = byKey[codexModelKey(m.ID)]
		}
		if base == nil {
			if missing == "" {
				missing = m.ID
			}
			continue
		}
		entry := cloneCodexMetadata(base).(map[string]any)
		slug, label := menuWire(p, m.ID), m.Label+" · "+p.Name
		entry["slug"], entry["display_name"] = slug, label
		entry["visibility"], entry["supported_in_api"], entry["priority"] = "list", true, len(out)
		entry["service_tiers"], entry["additional_speed_tiers"] = []any{}, []any{}
		entry["upgrade"], entry["availability_nux"] = nil, nil
		out = append(out, entry)
		nativeSlugs = append(nativeSlugs, base["slug"].(string))
		listed[slug] = true
		labels[label]++
	}
	if len(out) == 0 {
		return nil, missing, nil
	}
	for _, entry := range out {
		if labels[entry["display_name"].(string)] > 1 {
			entry["display_name"] = entry["slug"]
		}
	}
	for i, native := range nativeSlugs {
		if listed[native] {
			continue
		}
		alias := cloneCodexMetadata(out[i]).(map[string]any)
		alias["slug"], alias["visibility"] = native, "hide"
		out = append(out, alias)
		listed[native] = true
	}
	body, err := json.Marshal(map[string]any{"models": out})
	return body, missing, err
}

func codexModelKey(slug string) string {
	slug = strings.TrimPrefix(slug, "system.ai.")
	slug = strings.TrimPrefix(slug, "databricks-")
	return strings.ReplaceAll(slug, ".", "-")
}

func cloneCodexMetadata(value any) any {
	switch value := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(value))
		for key, item := range value {
			out[key] = cloneCodexMetadata(item)
		}
		return out
	case []any:
		out := make([]any, len(value))
		for i, item := range value {
			out[i] = cloneCodexMetadata(item)
		}
		return out
	default:
		return value
	}
}
