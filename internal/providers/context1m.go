package providers

import (
	"strings"

	"github.com/edotau/claude-code/internal/models"
)

// Serves1M reports whether p's endpoint serves the 1M window; the endpoint type alone decides.
func (p *Provider) Serves1M() bool {
	if p == nil {
		return false
	}
	switch p.SpellingRule() {
	case "databricks-serving", "anthropic":
		return true
	}
	return false
}

// ContextWindow is the window Claude Code runs id at on p: 1M when typed [1m] or a catalog Claude model on a 1M endpoint.
func ContextWindow(p *Provider, id string) int {
	if strings.HasSuffix(id, oneMMarker) || p.Serves1M() {
		return models.ContextWindow(id)
	}
	return models.ContextWindowStd
}
