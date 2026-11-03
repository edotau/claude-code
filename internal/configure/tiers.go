// tiers.go — the subscription tier each agent's own sign-in reports, shown as "subscription (Max 20x)".
package configure

// WithTier renders a mode with its tier when one is known: "subscription (Max 20x)".
func WithTier(mode, tier string) string {
	if tier == "" {
		return mode
	}
	return mode + " (" + tier + ")"
}
