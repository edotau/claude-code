package providers

import (
	"regexp"
	"strings"

	"github.com/edotau/claude-code/internal/models"
)

// quotaRE covers every upstream phrasing for "this key cannot use this model/account at all" (plan/quota
// exhausted or a hard 402), as opposed to a transient rate limit worth waiting out: Gemini free tier
// ("limit: 0"), OpenAI ("insufficient_quota"), a 402 body, or "usage limit ... try again at <time>".
var quotaRE = regexp.MustCompile(`\b402\b|payment required|exceeded your (?:monthly|current|weekly) (?:quota|limit)|` +
	`usage limit|insufficient_quota|limit: 0|quota exceeded for metric|out of (?:premium )?(?:credits|requests)|monthly quota`)

// IsQuota reports whether a 429 body or error text denies the model/account outright (credits, plan window,
// zero quota), not a transient rate limit worth waiting out; the caller should demote to the provider's next
// slot, or mark the account exhausted, instead of retrying.
func IsQuota(text string) bool { return quotaRE.MatchString(strings.ToLower(text)) }

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
