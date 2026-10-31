package providers

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/edotau/claude-code/internal/models"
)

// FallbackModels covers the two providers whose live catalog can fail for a known reason: subscription
// (passthrough, no API key) falls back to the registry's configured Claude models; chatgpt (no /models
// route) falls back to codex's local model cache. Any other provider's error is returned unchanged.
func FallbackModels(p *Provider, liveErr error) ([]ModelInfo, string, error) {
	if p.provider() == "openai" && p.Auth.Login == "codex" {
		if cached, cerr := CodexCachedModels(); cerr == nil && len(cached) > 0 {
			return cached, "chatgpt has no live /models route; showing codex's local model cache (~/.codex/models_cache.json, may be stale)", nil
		}
		return nil, "", fmt.Errorf("%v (run `codex` once to populate ~/.codex/models_cache.json, or pin a model id by hand: claude-code models --pin opus=chatgpt:<id>)", liveErr)
	}
	if p.Auth.Type == AuthCLI {
		if known := RegistryModels(p); len(known) > 0 {
			return known, fmt.Sprintf("live catalog needs an API key (%v); showing the registry's known Claude models", liveErr), nil
		}
	}
	return nil, "", liveErr
}

// RegistryModels is provider p's configured slot models, in slot order, each tagged with its use case.
func RegistryModels(p *Provider) []ModelInfo {
	var out []ModelInfo
	for _, s := range models.Slots {
		if id := p.Models[s]; id != "" {
			out = append(out, ModelInfo{ID: id, Name: s, UseCase: models.UseCase(id, s)})
		}
	}
	return out
}

// CodexCachedModels reads codex's models_cache.json under $CODEX_HOME (default ~/.codex); never a network call.
func CodexCachedModels() ([]ModelInfo, error) {
	dir := os.Getenv("CODEX_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		dir = filepath.Join(home, ".codex")
	}
	b, err := os.ReadFile(filepath.Join(dir, "models_cache.json"))
	if err != nil {
		return nil, err
	}
	var doc struct {
		Models []struct {
			Slug        string `json:"slug"`
			DisplayName string `json:"display_name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	out := make([]ModelInfo, len(doc.Models))
	for i, m := range doc.Models {
		out[i] = ModelInfo{ID: m.Slug, Name: m.DisplayName, UseCase: models.UseCase(m.Slug, m.DisplayName)}
	}
	return out, nil
}

// modelDeniedRE covers the narrow markers meaning "this key cannot use this MODEL at all" (a lower slot on
// the same account may still serve): Gemini free tier ("limit: 0"), OpenAI ("insufficient_quota").
var modelDeniedRE = regexp.MustCompile(`limit: 0|exceeded your current quota|insufficient_quota|quota exceeded for metric`)

// accountRE covers the broader markers meaning "this ACCOUNT is exhausted" (plan window, credits, a hard
// 402), as opposed to a transient rate limit worth waiting out: a 402 body, "usage limit ... try again at
// <time>", or a monthly/weekly plan quota.
var accountRE = regexp.MustCompile(`\b402\b|payment required|exceeded your (?:monthly|current|weekly) (?:quota|limit)|` +
	`usage limit|out of (?:premium )?(?:credits|requests)|(?:monthly|weekly) quota|` +
	`you've hit your (?:session |weekly |5-hour |monthly (?:spend )?|team's shared |channel's monthly spend )?(?:limit|budget)|out of usage credits`)

// hardAccountRE is accountRE's subset ModelDenied vetoes on: a 402, "payment required", "usage limit", a
// named monthly/weekly plan quota, or credits out. Deliberately drops accountRE's "exceeded your current
// quota/limit" — Gemini's free-tier per-model denial reuses that exact phrase alongside "limit: 0"
// (modelDeniedRE), and that one is a MODEL-specific denial, not account-wide exhaustion.
var hardAccountRE = regexp.MustCompile(`\b402\b|payment required|exceeded your (?:monthly|weekly) (?:quota|limit)|` +
	`usage limit|out of (?:premium )?(?:credits|requests)|(?:monthly|weekly) quota|` +
	`you've hit your (?:session |weekly |5-hour |monthly (?:spend )?|team's shared |channel's monthly spend )?(?:limit|budget)|out of usage credits`)

// ModelDenied reports whether a 429 body or error text denies this MODEL outright on the current key; the
// caller should demote to the provider's next slot instead of retrying the same model. Account-wide
// exhaustion (hardAccountRE: a 402, "payment required", a named plan window, or credits out) takes
// precedence — a body like "402 payment required: insufficient_quota" means the whole account is dead,
// not just this model, so demoting to a lower slot on the same exhausted account would only fail again.
func ModelDenied(text string) bool {
	low := strings.ToLower(text)
	return modelDeniedRE.MatchString(low) && !hardAccountRE.MatchString(low)
}

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
		if id := p.Models[next]; id != "" && !SameModel(id, model) {
			return Target{Provider: p, Model: id}, true
		}
	}
	return Target{}, false
}
