package copilot

import (
	"context"
	"time"

	"github.com/edotau/claude-code/internal/providers"
)

// DiscoverResult is one profile's `models discover` outcome.
type DiscoverResult struct {
	Provider     string                `json:"provider"`
	Profile      string                `json:"profile"`
	Source       string                `json:"source"`
	Discovered   int                   `json:"discovered"`
	Additions    []providers.ModelInfo `json:"additions"`
	Destinations []string              `json:"destinations"`
	Status       string                `json:"status"`
	Note         string                `json:"note,omitempty"`
}

// DiscoverPickerProfile live-discovers p's models and previews (or, with apply, appends) the saved picker additions.
func DiscoverPickerProfile(p *providers.Provider, apply bool) DiscoverResult {
	r := DiscoverResult{Provider: p.Provider, Profile: p.Name, Additions: []providers.ModelInfo{}, Destinations: []string{}, Status: "skipped"}
	source, skip := DiscoverySource(p)
	r.Source = source
	if skip != "" {
		r.Note = skip
		return r
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	live, err := DiscoverProfileModels(ctx, p)
	if err != nil {
		r.Status = "error"
		r.Note = err.Error() + " (no fallback saved)"
		return r
	}
	r.Discovered = len(live)
	r.Destinations = discoverDestinations(p)
	var added []providers.ModelInfo
	if apply {
		added, err = providers.AppendPickerModels(p, live)
	} else {
		added, err = providers.PreviewPickerModels(p, live)
	}
	if err != nil {
		r.Status = "error"
		r.Note = "saved picker additions unavailable or invalid; nothing overwritten: " + err.Error()
		return r
	}
	if len(added) > 0 {
		r.Additions = added
	}
	r.Status = "preview"
	if apply {
		r.Status = "applied"
	}
	r.Note = "Listing is not inference proof; existing selections and native vendor catalogs are unchanged."
	return r
}

func discoverDestinations(p *providers.Provider) []string {
	if p.Provider == "github" {
		return []string{"sealed profile history only (Copilot pickers retain live policy filtering)"}
	}
	d := []string{"configure model picker", "agent model picker", "OpenCode harness-generated catalog (next routed launch)"}
	if p.Preset == providers.PresetDatabricks {
		d = append(d, "Codex generated catalog (Responses models with bundled metadata only)")
	}
	return d
}

// DiscoverPickerProfiles runs DiscoverPickerProfile over names; ok is false when one errored or none completed.
func DiscoverPickerProfiles(reg *providers.Registry, names []string, apply bool) (results []DiscoverResult, ok bool) {
	results = make([]DiscoverResult, 0, len(names))
	ok = true
	completed := 0
	for _, name := range names {
		p, _ := reg.Get(name)
		r := DiscoverPickerProfile(p, apply)
		if r.Status == "error" {
			ok = false
		}
		if r.Status == "applied" || r.Status == "preview" {
			completed++
		}
		results = append(results, r)
	}
	return results, ok && completed > 0
}
