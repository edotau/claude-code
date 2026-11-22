package providers

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/vault"
)

// opencodeMenuKey is the vault Overlay entry holding the user's menu: a JSON array of savedMenuEntry.
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

// menuCache holds the vault menu per vault.db stamp; vaultOpencodeMenu hands out clones.
var menuCache = paths.NewStampCache[[]OpencodeModel]("providers.opencodemenu")

// warnedMenu limits the unusable-overlay warning to one per process.
var warnedMenu sync.Once

func vaultOpencodeMenu() ([]OpencodeModel, bool) {
	out, err := menuCache.Get(vault.Path(), func() (out []OpencodeModel, err error) {
		err = vault.View(func(t *vault.Tx) error {
			if raw, ok := t.Get(vault.Overlay, opencodeMenuKey); ok && raw != "" {
				var saved []savedMenuEntry
				if err := json.Unmarshal([]byte(raw), &saved); err != nil {
					warnMenu(fmt.Errorf("vault overlay %s is not a JSON menu: %w", opencodeMenuKey, err))
					return nil
				}
				out = loadSavedMenu(saved)
			}
			return nil
		})
		return out, err
	})
	if err != nil {
		warnMenu(err)
	}
	return slices.Clone(out), err == nil && len(out) > 0
}

func warnMenu(err error) {
	warnedMenu.Do(func() { fmt.Fprintf(os.Stderr, "warning: %v; using the built-in opencode menu\n", err) })
}

// savedMenuEntry is the stored form: Responses is set only where the user overrode the catalog.
type savedMenuEntry struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Responses *bool  `json:"responses,omitempty"`
}

// loadSavedMenu resolves each entry's Responses: chat-only ids never, else the saved override, else the catalog.
func loadSavedMenu(saved []savedMenuEntry) []OpencodeModel {
	out := make([]OpencodeModel, 0, len(saved))
	for _, e := range saved {
		r := models.ResponsesOnly(e.ID)
		if e.Responses != nil {
			r = *e.Responses
		}
		out = append(out, OpencodeModel{ID: e.ID, Label: e.Label, Responses: r && !models.ChatOnly(e.ID)})
	}
	return out
}

// saveMenu keeps Responses only where it differs from the catalog, so a catalog change reaches saved entries.
func saveMenu(menu []OpencodeModel) []savedMenuEntry {
	out := make([]savedMenuEntry, 0, len(menu))
	for _, m := range menu {
		e := savedMenuEntry{ID: m.ID, Label: m.Label}
		if m.Responses != models.ResponsesOnly(m.ID) && !models.ChatOnly(m.ID) {
			e.Responses = &m.Responses
		}
		out = append(out, e)
	}
	return out
}

// SetOpencodeMenu stores menu as the vault overlay; an empty menu deletes the entry (back to the seed).
func SetOpencodeMenu(menu []OpencodeModel) error {
	raw := ""
	if len(menu) > 0 {
		b, err := json.Marshal(saveMenu(menu))
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
	{ID: "gpt-6-sol", Label: "GPT-6 Sol", Responses: true},
	{ID: "gpt-6-astra", Label: "GPT-6 Astra", Responses: true},
	{ID: "gpt-5-5", Label: "GPT 5.5", Responses: true},
}

// MergePickerModels preserves existing labels and order and appends new exact IDs.
func MergePickerModels(existing, additions []ModelInfo) []ModelInfo {
	out := slices.Clone(existing)
	seen := make(map[string]bool, len(out))
	for _, m := range out {
		seen[m.ID] = true
	}
	for _, m := range additions {
		if m.ID != "" && !seen[m.ID] {
			out = append(out, m)
			seen[m.ID] = true
		}
	}
	return out
}

// MenuID is id without the Databricks prefix, the spelling the global menu stores.
func MenuID(id string) string { return strings.TrimPrefix(models.Strip1M(id), "databricks-") }

// MenuIDSet is the menu spellings of ids, so a bare and a databricks-prefixed id match.
func MenuIDSet(ids []string) map[string]bool {
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[MenuID(id)] = true
	}
	return out
}

// DropFromMenu removes ids from the global saved menu overlay and returns those it held; write false only reports.
func DropFromMenu(ids []string, write bool) (removed []string, err error) {
	menu, ok := vaultOpencodeMenu()
	if !ok {
		return nil, nil
	}
	want := MenuIDSet(ids)
	var kept []OpencodeModel
	for _, m := range menu {
		if want[MenuID(m.ID)] {
			removed = append(removed, m.ID)
			continue
		}
		kept = append(kept, m)
	}
	if !write || len(removed) == 0 {
		return removed, nil
	}
	return removed, SetOpencodeMenu(kept)
}
