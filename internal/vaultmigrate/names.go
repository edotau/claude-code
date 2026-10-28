package vaultmigrate

import (
	"regexp"
	"strings"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
)

// personalProfile is the github row named by role; the owner login is its login field.
const personalProfile = "personal"

// retiredProfiles maps a retired built-in profile name to its replacement (pin values, overlay names).
var retiredProfiles = map[string]string{"subscription": "max", "github-copilot": personalProfile, "gemini": "antigravity"}

// profilePins hold a registry profile name as their value; modelPins hold [profile:]model.
var (
	profilePins = []string{providers.PinProfile, providers.PinCodexProfile, providers.PinGeminiProfile, providers.PinOpencodeProfile}
	oldSlotPin  = regexp.MustCompile(`^HARNESS_([A-Z]+)_MODEL$`)
)

// newPinName is the current name of pin key (key itself when it never changed).
func newPinName(key string) string {
	switch key {
	case "HARNESS_PROVIDER":
		return providers.PinProfile
	case "HARNESS_MODEL":
		return providers.PinModel
	case "COPILOT_GITHUB_USER":
		return providers.PinCopilotProfile
	case "COPILOT_CONTEXT_TIER":
		return providers.PinCopilotContext
	case "GEMINI_AGENT_MODEL":
		return providers.PinGeminiModel
	}
	if m := oldSlotPin.FindStringSubmatch(key); m != nil {
		for _, slot := range models.Slots {
			if strings.EqualFold(slot, m[1]) {
				return providers.SlotPin(slot)
			}
		}
	}
	if base, ok := strings.CutSuffix(key, "_REASONING_EFFORT"); ok {
		return base + "_EFFORT"
	}
	return key
}

// isModelPin reports whether the (new-named) pin holds [profile:]model.
func isModelPin(key string) bool {
	if key == providers.PinModel {
		return true
	}
	for _, slot := range models.Slots {
		if key == providers.SlotPin(slot) {
			return true
		}
	}
	return false
}

// newPinValue maps a retired profile name in a (new-named) pin's value to its replacement.
func newPinValue(key, val string) string {
	for _, k := range profilePins {
		if key == k {
			return retire(val)
		}
	}
	if isModelPin(key) {
		if prof, model, ok := strings.Cut(val, ":"); ok {
			return retire(prof) + ":" + model
		}
	}
	return val
}

func retire(name string) string {
	if to, ok := retiredProfiles[strings.TrimSpace(name)]; ok {
		return to
	}
	return name
}

// oldSecretPrefixes are the pre-rename secret prefixes of a profile: its own name, then each retired name it replaced.
func oldSecretPrefixes(profile string) []string {
	out := []string{oldSecretPrefix(profile)}
	for old, to := range retiredProfiles {
		if to == profile {
			out = append(out, oldSecretPrefix(old))
		}
	}
	return out
}

var secretIDRE = regexp.MustCompile(`[^A-Z0-9]+`)

// oldSecretPrefix is the pre-rename PROVIDER_<ID> secret prefix of a profile.
func oldSecretPrefix(profile string) string {
	return "PROVIDER_" + strings.Trim(secretIDRE.ReplaceAllString(strings.ToUpper(profile), "_"), "_")
}

// copilotLogin returns the COPILOT_PROFILE pin value when it names a login rather than a github profile,
// and the profile whose login field (or, with none set, the personal row) it belongs to.
func copilotLogin(pins map[string]string, reg *providers.Registry) (login, profile string) {
	v := pins[providers.PinCopilotProfile]
	if v == "" {
		v = pins["COPILOT_GITHUB_USER"]
	}
	if v == "" {
		return "", ""
	}
	if p := reg.Providers[v]; p != nil && p.Provider == "github" {
		return "", ""
	}
	for name, p := range reg.Providers {
		if p.Provider == "github" && p.Login == v {
			return "", name // already recorded on a row: only the pin is rewritten
		}
	}
	if p := reg.Providers[personalProfile]; p != nil && p.Provider == "github" && p.Login == "" {
		return v, personalProfile
	}
	return "", ""
}
