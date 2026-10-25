package profile

import (
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
)

// vendorPins maps a profile agent to the pin that names its model.
var vendorPins = []struct{ agent, pin string }{
	{"copilot", providers.PinCopilotModel},
	{"codex", providers.PinCodexModel},
	{"gemini", providers.PinGeminiModel},
}

// Snapshot captures today's effective bindings: the session selection plus the vendor model pins.
func Snapshot(reg *providers.Registry) (providers.Profile, error) {
	sel, err := providers.Select(reg, "")
	if err != nil {
		return nil, err
	}
	row := providers.AgentRow{Account: sel.Provider.Name, Models: map[string]string{}}
	for _, slot := range models.Slots {
		if m := sel.ClientModel(slot); m != "" {
			row.Models[slot] = m
		}
	}
	set := providers.Profile{"session": row}
	for _, v := range vendorPins {
		if m := providers.Pin(v.pin); m != "" {
			set[v.agent] = providers.AgentRow{Model: m}
		}
	}
	return set, nil
}
