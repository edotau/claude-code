package providers

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
)

// FallbackModels serves subscription from the registry's Claude models and chatgpt from codex's cache.
func FallbackModels(p *Provider, liveErr error) ([]ModelInfo, string, error) {
	if p.Provider == "openai" && p.Auth.Login == "codex" {
		if cached, cerr := codexCachedModels(); cerr == nil && len(cached) > 0 {
			return cached, "chatgpt has no live /models route; showing codex's local model cache (~/.codex/models_cache.json, may be stale)", nil
		}
		return nil, "", fmt.Errorf("%w (run `codex` once to populate ~/.codex/models_cache.json, or pin a model id by hand: claude-code models --pin opus=chatgpt:<id>)", liveErr)
	}
	if p.Auth.Type == AuthCLI {
		if known := registryModels(p); len(known) > 0 {
			return known, fmt.Sprintf("live catalog needs an API key (%v); showing the registry's known Claude models", liveErr), nil
		}
	}
	return nil, "", liveErr
}

// registryModels is provider p's configured alias models, in alias order, each tagged with its use case.
func registryModels(p *Provider) []ModelInfo {
	var out []ModelInfo
	for _, s := range models.AliasNames {
		if id := p.Models[s]; id != "" {
			out = append(out, ModelInfo{ID: id, Name: s, UseCase: models.UseCase(id, s)})
		}
	}
	return out
}

// codexCachedModels reads codex's models_cache.json under $CODEX_HOME (default ~/.codex); never a network call.
func codexCachedModels() ([]ModelInfo, error) {
	dir, err := paths.CodexHome()
	if err != nil {
		return nil, err
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

// modelDeniedRE covers the narrow markers meaning "this key cannot use this MODEL at all" (a lower alias on
// the same account may still serve): Gemini free tier ("limit: 0"), OpenAI ("insufficient_quota").
var modelDeniedRE = regexp.MustCompile(`limit: 0|exceeded your current quota|insufficient_quota|quota exceeded for metric`)

// accountTail is the account-exhaustion alternation shared by accountRE and hardAccountRE.
const accountTail = `usage limit|out of (?:premium )?(?:credits|requests)|(?:monthly|weekly) quota|` +
	`you've hit your (?:session |weekly |5-hour |monthly (?:spend )?|team's shared |channel's monthly spend )?(?:limit|budget)|out of usage credits`

// accountRE covers markers meaning this ACCOUNT is exhausted (plan window, credits, a 402), not a rate limit.
var accountRE = regexp.MustCompile(`(?:http|status|code)[ :=]*402\b|payment required|exceeded your (?:monthly|current|weekly) (?:quota|limit)|` + accountTail)

// hardAccountRE is accountRE's subset ModelDenied vetoes on: a 402, "payment required", "usage limit", a
// named monthly/weekly plan quota, or credits out. Deliberately drops accountRE's "exceeded your current
// quota/limit" — Gemini's free-tier per-model denial reuses that exact phrase alongside "limit: 0"
// (modelDeniedRE), and that one is a MODEL-specific denial, not account-wide exhaustion.
var hardAccountRE = regexp.MustCompile(`(?:http|status|code)[ :=]*402\b|payment required|exceeded your (?:monthly|weekly) (?:quota|limit)|` + accountTail)

// ModelDenied reports whether text denies this MODEL on the key (demote); hardAccountRE exhaustion vetoes it.
func ModelDenied(text string) bool {
	low := strings.ToLower(text)
	return modelDeniedRE.MatchString(low) && !hardAccountRE.MatchString(low)
}

// IsQuota reports whether text means the account is exhausted (credits, plan window, model denial), not a rate limit.
func IsQuota(text string) bool {
	low := strings.ToLower(text)
	return accountRE.MatchString(low) || modelDeniedRE.MatchString(low)
}

// Demote returns p's next lower alias target below model (opus → sonnet → haiku); ok is false at the bottom
// or when p has no distinct model there. Skipping models the caller already knows are dead is its job.
func Demote(p *Provider, model string) (Target, bool) {
	alias := p.AliasOf(model)
	if alias == "" {
		alias = models.Opus
	}
	for next := models.NextAlias(alias); next != ""; next = models.NextAlias(next) {
		if id := p.Models[next]; id != "" && !sameModel(id, model) {
			return Target{Provider: p, Model: id}, true
		}
	}
	return Target{}, false
}
