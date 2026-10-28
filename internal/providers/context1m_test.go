package providers

import (
	"testing"

	"github.com/edotau/claude-code/internal/models"
)

// The endpoint type alone decides who serves 1M: serving and first-party yes, ai-gateway and others no.
func TestServes1MByEndpointType(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    *Provider
		want bool
	}{
		{"nil", nil, false},
		{"databricks serving", &Provider{ModelPrefix: models.DatabricksPrefix}, true},
		{"first-party anthropic", &Provider{Provider: "anthropic"}, true},
		{"ai-gateway", &Provider{ModelPrefix: gatewayModelPrefix}, false},
		{"github copilot", &Provider{Provider: "github"}, false},
		{"non-Claude endpoint", &Provider{Provider: "ollama"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.p.Serves1M(); got != tc.want {
				t.Errorf("Serves1M = %v, want %v", got, tc.want)
			}
		})
	}
}

// ContextWindow: a typed [1m] is always 1M; a bare catalog id is 1M only on a 1M endpoint; a non-Claude id is 200K.
func TestContextWindowByEndpoint(t *testing.T) {
	serving := &Provider{ModelPrefix: models.DatabricksPrefix}
	gateway := &Provider{ModelPrefix: gatewayModelPrefix}
	copilot := &Provider{Provider: "github"}
	for _, tc := range []struct {
		name string
		p    *Provider
		id   string
		want int
	}{
		{"serving 5.5", serving, "claude-opus-5-5", models.ContextWindow1M},
		{"gateway 5.5 bare", gateway, "claude-opus-5-5", models.ContextWindowStd},
		{"gateway typed [1m] kept", gateway, "claude-opus-5-5[1m]", models.ContextWindow1M},
		{"copilot bare", copilot, "claude-opus-5-5", models.ContextWindowStd},
		{"copilot typed [1m]", copilot, "claude-opus-5-5[1m]", models.ContextWindow1M},
		{"serving non-Claude", serving, "gpt-5", models.ContextWindowStd},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ContextWindow(tc.p, tc.id); got != tc.want {
				t.Errorf("ContextWindow(%q) = %d, want %d", tc.id, got, tc.want)
			}
		})
	}
}
