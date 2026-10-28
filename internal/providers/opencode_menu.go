package providers

import (
	"encoding/json"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/vault"
)

// opencodeMenuKey is the vault Overlay entry holding the user's menu: a JSON array of OpencodeModel.
const opencodeMenuKey = "opencode-models"

// OpencodeMenu is the menu opencode (and codex's /model) shows: the vault overlay if set and valid, else the seed.
func OpencodeMenu() []OpencodeModel {
	if m, ok := vaultOpencodeMenu(); ok {
		return m
	}
	return DatabricksOpencodeMenu
}

// OpencodeMenuFromVault reports whether OpencodeMenu is served from the vault overlay rather than the seed.
func OpencodeMenuFromVault() bool {
	_, ok := vaultOpencodeMenu()
	return ok
}

func vaultOpencodeMenu() ([]OpencodeModel, bool) {
	var out []OpencodeModel
	var found bool
	err := vault.View(func(t *vault.Tx) error {
		raw, ok := t.Get(vault.Overlay, opencodeMenuKey)
		if !ok || raw == "" {
			return nil
		}
		found = json.Unmarshal([]byte(raw), &out) == nil && len(out) > 0
		return nil
	})
	if err != nil || !found {
		return nil, false
	}
	return out, true
}

// SetOpencodeMenu stores menu as the vault overlay; an empty menu deletes the entry (back to the seed).
func SetOpencodeMenu(menu []OpencodeModel) error {
	raw := ""
	if len(menu) > 0 {
		b, err := json.Marshal(menu)
		if err != nil {
			return err
		}
		raw = string(b)
	}
	return vault.Update(func(t *vault.Tx) error { return t.Put(vault.Overlay, opencodeMenuKey, raw) })
}

// OpencodeModel is one /models picker entry: a canonical id, its label, and whether it needs the Responses API.
type OpencodeModel struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Responses bool   `json:"responses,omitempty"`
}

// DatabricksOpencodeMenu is the curated multi-family menu opencode shows on a databricks preset row.
var DatabricksOpencodeMenu = []OpencodeModel{
	{ID: "claude-opus-5-5", Label: "Claude Opus 5.5"},
	{ID: "claude-sonnet-5-5", Label: "Claude Sonnet 5.5"},
	{ID: "claude-haiku-5-5", Label: "Claude Haiku 5.5"},
	{ID: "gemini-3-8-flash", Label: "Gemini 3.8 Flash"},
	{ID: "gemini-3-7-flash", Label: "Gemini 3.7 Flash"},
	{ID: "kimi-k3", Label: "Kimi K3"},
	{ID: "deepseek-v4-pro-0813", Label: "DeepSeek V4 Pro 0813"},
	{ID: "glm-5-3", Label: "GLM 5.3"},
	{ID: "glm-5-3-flash", Label: "GLM 5.3 Flash"},
	{ID: "grok-4-7", Label: "Grok 4.7"},
	{ID: "grok-4-6", Label: "Grok 4.6"},
	{ID: "qwen35-122b-a10b", Label: "Qwen 3.5 122B A10B"},
	{ID: "qwen3-next-80b-a3b-instruct", Label: "Qwen3 Next 80B A3B Instruct"},
	{ID: "llama-4-maverick", Label: "Llama 4 Maverick"},
	{ID: "gpt-oss-120b", Label: "GPT OSS 120B"},
	{ID: "gpt-6-1-sol", Label: "GPT-6.1 Sol", Responses: true},
	{ID: "gpt-6-astra", Label: "GPT-6 Astra", Responses: true},
	{ID: "gpt-5-5", Label: "GPT 5.5", Responses: true},
}

// FirstResponsesModel is the first Responses-capable OpencodeMenu entry spelled for p, or "" when none.
func FirstResponsesModel(p *Provider) string {
	for _, m := range OpencodeMenu() {
		if m.Responses {
			return p.WireModel(m.ID)
		}
	}
	return ""
}

// ServesResponses reports whether p serves model on Responses: its menu entry's flag when listed, else the catalog.
func ServesResponses(p *Provider, model string) bool {
	for _, m := range OpencodeMenu() {
		if p.WireModel(m.ID) == model {
			return m.Responses
		}
	}
	return models.ResponsesOnly(model)
}

// ResponsesID is ServesResponses for a bare menu id: the entry's flag when listed, else the catalog.
func ResponsesID(id string) bool {
	for _, m := range OpencodeMenu() {
		if m.ID == id {
			return m.Responses
		}
	}
	return models.ResponsesOnly(id)
}
