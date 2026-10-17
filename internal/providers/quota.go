package providers

import (
	"strings"

	"github.com/edotau/claude-code/internal/models"
)

// hardQuotaMarkers are the upstream phrasings for "this key cannot use this model at all" (plan/quota exhausted),
// as opposed to a transient rate limit worth waiting out: Gemini free tier ("limit: 0"), OpenAI ("insufficient_quota").
var hardQuotaMarkers = []string{"limit: 0", "exceeded your current quota", "insufficient_quota", "quota exceeded for metric"}

// HardQuota reports whether a 429 body or error text denies the model outright; the caller should demote to
// the provider's next slot instead of retrying the same model.
func HardQuota(text string) bool {
	low := strings.ToLower(text)
	for _, m := range hardQuotaMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

// Demote returns p's next lower slot target below model (opus → sonnet → haiku); ok is false at the bottom
// or when p has no distinct model there. Skipping models the caller already knows are dead is its job.
func Demote(p *Provider, model string) (Target, bool) {
	slot := p.SlotOf(model)
	if slot == "" {
		slot = models.Opus
	}
	for next := models.NextSlot(slot); next != ""; next = models.NextSlot(next) {
		if id := p.Models[next]; id != "" && id != model {
			return Target{Provider: p, Model: id}, true
		}
	}
	return Target{}, false
}
