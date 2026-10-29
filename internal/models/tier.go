package models

// Effort tiers; the strings equal the hooks package's TierLow/Medium/High.
const (
	EffortLow    = "low"
	EffortMedium = "medium"
	EffortHigh   = "high"
)

// TierModel is the Agent-tool alias for an effort tier ("" for an unknown tier); an alias, so `models --pin` applies.
func TierModel(tier string) string {
	switch tier {
	case EffortLow:
		return Haiku
	case EffortMedium, EffortHigh:
		return Sonnet
	}
	return ""
}
