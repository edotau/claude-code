package providers

import (
	"regexp"
	"strings"

	"github.com/edotau/claude-code/internal/models"
)

// modelDeniedRE covers the narrow markers meaning "this key cannot use this MODEL at all" (a lower slot on
// the same account may still serve): Gemini free tier ("limit: 0"), OpenAI ("insufficient_quota").
var modelDeniedRE = regexp.MustCompile(`limit: 0|exceeded your current quota|insufficient_quota|quota exceeded for metric`)

// accountRE covers the broader markers meaning "this ACCOUNT is exhausted" (plan window, credits, a hard
// 402), as opposed to a transient rate limit worth waiting out: a 402 body, "usage limit ... try again at
// <time>", or a monthly/weekly plan quota.
var accountRE = regexp.MustCompile(`\b402\b|payment required|exceeded your (?:monthly|current|weekly) (?:quota|limit)|` +
	`usage limit|out of (?:premium )?(?:credits|requests)|monthly quota`)

// ModelDenied reports whether a 429 body or error text denies this MODEL outright on the current key; the
// caller should demote to the provider's next slot instead of retrying the same model.
func ModelDenied(text string) bool { return modelDeniedRE.MatchString(strings.ToLower(text)) }

// IsQuota reports whether a 429/402 body or error text means the account is exhausted (credits, plan
// window, or a model-denial that leaves no lower slot to try) rather than a transient rate limit worth
// waiting out; the caller should mark the account exhausted instead of retrying.
func IsQuota(text string) bool {
	low := strings.ToLower(text)
	return accountRE.MatchString(low) || modelDeniedRE.MatchString(low)
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
