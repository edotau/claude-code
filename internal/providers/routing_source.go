package providers

// RouteWeightsSource reports "vault" when the vault overlay's own routing object sets weights, else "default".
func RouteWeightsSource() string {
	overlay, err := RoutingOverlay()
	if err != nil || overlay.Weights == nil {
		return "default"
	}
	return "vault"
}
