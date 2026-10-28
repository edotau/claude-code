package providers

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

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

// CodexCachedModels reads codex's own ~/.codex/models_cache.json (written by the codex CLI on login/use);
// never a network call, so it's a cheap, always-available stand-in for chatgpt's missing /models route.
func CodexCachedModels() ([]ModelInfo, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(home, ".codex", "models_cache.json"))
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
