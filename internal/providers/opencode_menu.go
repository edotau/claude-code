package providers

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/textutil"
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
var menuCache paths.StampCache[[]OpencodeModel]

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

// pickerCatalog stores profile-scoped selectable history, not current availability.
type pickerCatalog struct {
	Schema int         `json:"schema"`
	Scope  string      `json:"scope"`
	Models []ModelInfo `json:"models"`
	// Dropped ids are never re-added by discovery until `models discover --undrop`.
	Dropped []string `json:"dropped,omitempty"`
	// DroppedWhy maps a dropped id to "deprecated <date>"; absent for a hand drop.
	DroppedWhy map[string]string `json:"dropped_why,omitempty"`
}

var _ = schemas.Register(schemas.Format{
	Name: "picker-additions", Owner: "internal/providers", Kind: schemas.Store,
	Glob: vault.Path, Version: 1,
	Keep: "profile-scoped sealed picker-additions/v1 records inside the vault (additions, dropped ids); not live availability",
})

func pickerScope(p *Provider) string {
	// Hash configuration without exposing auth commands or headers; models/pins are not scope.
	opts := p.Options
	opts.WarehouseID, opts.Prices = "", nil // cost settings are not routing: setting them must not orphan the drop list
	routes := p.Routes
	if host := PresetHost(p.Host); p.Preset == PresetDatabricks && host != "" && routes[ProtocolGemini] == host+"/serving-endpoints/gemini" {
		routes = maps.Clone(routes)
		delete(routes, ProtocolGemini) // preset-derived default: omitting it preserves existing sealed drop hashes
	}
	data, _ := json.Marshal([]any{p.Name, p.Provider, p.Login, p.Identity,
		p.Protocol, p.BaseURL, routes, p.Host, p.CLIProfile, p.SpellingRule(), opts,
		p.Auth, p.Headers})
	return textutil.SHA256Hex(data)
}

func pickerKey(p *Provider) string { return "picker-additions/v1/" + pickerScope(p) }

func decodePicker(raw, scope string) (pickerCatalog, error) {
	var c pickerCatalog
	if json.Unmarshal([]byte(raw), &c) != nil || c.Schema != 1 || c.Scope != scope {
		return c, errors.New("invalid picker additions schema or scope; refusing to overwrite")
	}
	for _, m := range c.Models {
		if m.ID == "" {
			return c, errors.New("invalid picker additions entry; refusing to overwrite")
		}
	}
	return c, nil
}

// pickerRecord reads this profile's record; only a missing record counts as empty.
func pickerRecord(p *Provider) (pickerCatalog, error) {
	raw, found, err := vault.ReadSecret(pickerKey(p))
	if err != nil {
		return pickerCatalog{}, errors.New("cannot read or decrypt saved picker additions")
	}
	if !found {
		return pickerCatalog{}, nil
	}
	return decodePicker(raw, pickerScope(p))
}

// PickerModels reads this profile's additions; only a missing record counts as empty.
func PickerModels(p *Provider) ([]ModelInfo, error) {
	c, err := pickerRecord(p)
	return c.Models, err
}

// PickerDropped reads the ids dropped from this profile's picker.
func PickerDropped(p *Provider) ([]string, error) {
	c, err := pickerRecord(p)
	return c.Dropped, err
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

// pickerNewWithBaseline excludes configured aliases and the baseline menu using known wire equivalence.
func pickerNewWithBaseline(p *Provider, saved, live []ModelInfo, baseline []OpencodeModel, dropped []string) []ModelInfo {
	seen := pickerSeen(p, saved, baseline)
	drop := idSet(dropped)
	var out []ModelInfo
	for _, m := range live {
		if m.ID == "" || drop[MenuID(m.ID)] || seen[p.WireModel(models.Strip1M(m.ID))] {
			continue
		}
		if p.Preset == PresetDatabricks && (!m.ReadyChat() || llama3Instruct(m.ID)) {
			continue
		}
		seen[p.WireModel(models.Strip1M(m.ID))] = true
		// Do not persist readiness or capabilities as a claim of current availability.
		out = append(out, ModelInfo{ID: m.ID, Name: m.Name, UseCase: m.UseCase})
	}
	slices.SortFunc(out, func(a, b ModelInfo) int { return cmp.Compare(a.ID, b.ID) })
	return out
}

// llama3Instruct is a llama 3.x instruct id, kept out of the catalog because it answers tool tasks wrong.
func llama3Instruct(id string) bool {
	id = strings.TrimPrefix(strings.ToLower(id), "databricks-")
	return strings.HasPrefix(id, "meta-llama-3-") && strings.HasSuffix(id, "-instruct")
}

// pickerSeen is the set of wire ids already offered: configured aliases, the legacy menu (Databricks), saved additions.
func pickerSeen(p *Provider, saved []ModelInfo, baseline []OpencodeModel) map[string]bool {
	seen := make(map[string]bool)
	for _, id := range p.Models {
		seen[p.WireModel(models.Strip1M(id))] = true
	}
	if p.Preset == PresetDatabricks {
		for _, m := range baseline {
			seen[p.WireModel(m.ID)] = true
		}
	}
	for _, m := range saved {
		seen[p.WireModel(models.Strip1M(m.ID))] = true
	}
	return seen
}

// PreviewPickerModels computes additions without writing any discovery state.
func PreviewPickerModels(p *Provider, live []ModelInfo) ([]ModelInfo, error) {
	c, err := pickerRecord(p)
	if err != nil {
		return nil, err
	}
	return pickerNewWithBaseline(p, c.Models, live, OpencodeMenu(), c.Dropped), nil
}

// AppendPickerModels atomically appends sealed additions without losing concurrent updates.
func AppendPickerModels(p *Provider, live []ModelInfo) (added []ModelInfo, err error) {
	name, scope := pickerKey(p), pickerScope(p)
	baseline := OpencodeMenu()
	err = vault.UpdateSealed(func(tx *vault.Tx, key *vault.Key) error {
		// UpdateSealed may retry after a concurrent key rotation.
		added = nil
		c, err := txPicker(tx, key, name, scope)
		if err != nil {
			return err
		}
		// Use the captured baseline to avoid a nested vault view inside this transaction.
		added = pickerNewWithBaseline(p, c.Models, live, baseline, c.Dropped)
		if len(added) == 0 {
			return nil
		}
		c.Models = MergePickerModels(c.Models, added)
		data, err := json.Marshal(c)
		if err != nil {
			return errors.New("cannot encode picker additions")
		}
		return tx.PutSealed(key, name, string(data), "picker additions", time.Now().UTC())
	})
	return added, err
}

// txPicker opens the sealed record inside a transaction; a missing record is an empty catalog.
func txPicker(tx *vault.Tx, key *vault.Key, name, scope string) (pickerCatalog, error) {
	record, found, err := tx.Record(name)
	if err != nil {
		return pickerCatalog{}, errors.New("cannot read saved picker additions record")
	}
	if !found {
		return pickerCatalog{Schema: 1, Scope: scope}, nil
	}
	raw, err := key.Open(name, record)
	if err != nil {
		return pickerCatalog{}, errors.New("cannot decrypt saved picker additions; refusing to overwrite")
	}
	return decodePicker(raw, scope)
}

// idSet is the menu spellings of ids, so a bare and a databricks-prefixed id match.
func idSet(ids []string) map[string]bool {
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[MenuID(id)] = true
	}
	return out
}

// applyDrop removes ids from c's additions and records them dropped; undrop clears the record instead.
func applyDrop(p *Provider, c pickerCatalog, ids []string, undrop bool) (next pickerCatalog, removed []string) {
	want := idSet(ids)
	next = c
	next.Schema, next.Scope = 1, pickerScope(p)
	if undrop {
		next.Dropped = slices.DeleteFunc(slices.Clone(c.Dropped), func(id string) bool { return want[MenuID(id)] })
		next.DroppedWhy = maps.Clone(c.DroppedWhy)
		maps.DeleteFunc(next.DroppedWhy, func(id, _ string) bool { return want[MenuID(id)] })
		return next, nil
	}
	next.Models = nil
	for _, m := range c.Models {
		if want[MenuID(m.ID)] {
			removed = append(removed, m.ID)
			continue
		}
		next.Models = append(next.Models, m)
	}
	next.Dropped = slices.Clone(c.Dropped)
	for _, id := range ids {
		if !slices.Contains(next.Dropped, id) {
			next.Dropped = append(next.Dropped, id)
		}
	}
	return next, removed
}

// EditPickerDrops drops (or, with undrop, un-drops) ids on p's picker; write false previews without storing.
// removed lists the saved additions the drop removes; the record is kept only for a Databricks row or a change.
func EditPickerDrops(p *Provider, ids []string, undrop, write bool) (removed []string, err error) {
	if !write {
		c, err := pickerRecord(p)
		if err != nil {
			return nil, err
		}
		_, removed = applyDrop(p, c, ids, undrop)
		return removed, nil
	}
	name, scope := pickerKey(p), pickerScope(p)
	err = vault.UpdateSealed(func(tx *vault.Tx, key *vault.Key) error {
		c, err := txPicker(tx, key, name, scope)
		if err != nil {
			return err
		}
		var next pickerCatalog
		next, removed = applyDrop(p, c, ids, undrop)
		unchanged := slices.Equal(c.Dropped, next.Dropped) && len(removed) == 0
		if unchanged || (!undrop && len(removed) == 0 && p.Preset != PresetDatabricks) {
			return nil
		}
		data, err := json.Marshal(next)
		if err != nil {
			return errors.New("cannot encode picker additions")
		}
		return tx.PutSealed(key, name, string(data), "picker additions", time.Now().UTC())
	})
	return removed, err
}

// RecordDropped drops id from p's picker with the reason and date; added is false when it was already dropped.
func RecordDropped(p *Provider, id, reason string, at time.Time) (added bool, err error) {
	name, scope := pickerKey(p), pickerScope(p)
	err = vault.UpdateSealed(func(tx *vault.Tx, key *vault.Key) error {
		added = false // UpdateSealed may retry
		c, err := txPicker(tx, key, name, scope)
		if err != nil || idSet(c.Dropped)[MenuID(id)] {
			return err
		}
		next, _ := applyDrop(p, c, []string{id}, false)
		next.DroppedWhy = maps.Clone(c.DroppedWhy)
		if next.DroppedWhy == nil {
			next.DroppedWhy = map[string]string{}
		}
		next.DroppedWhy[id] = reason + " " + at.UTC().Format(time.DateOnly)
		data, err := json.Marshal(next)
		if err != nil {
			return errors.New("cannot encode picker additions")
		}
		added = true
		return tx.PutSealed(key, name, string(data), "picker additions", time.Now().UTC())
	})
	return added, err
}

// DroppedReasons is p's recorded drop reasons by id ("deprecated 2026-11-20", "gateway-only 2026-11-21").
func DroppedReasons(p *Provider) (map[string]string, error) {
	c, err := pickerRecord(p)
	return c.DroppedWhy, err
}

// DroppedLive is the dropped ids of p that the live listing still serves, for discovery to report as skipped.
func DroppedLive(p *Provider, live []ModelInfo) ([]string, error) {
	c, err := pickerRecord(p)
	if err != nil || len(c.Dropped) == 0 {
		return nil, err
	}
	drop := idSet(c.Dropped)
	var out []string
	for _, m := range live {
		if drop[MenuID(m.ID)] {
			out = append(out, m.ID)
		}
	}
	return out, nil
}

// MenuID is id without the Databricks prefix, the spelling the global menu stores.
func MenuID(id string) string { return strings.TrimPrefix(models.Strip1M(id), "databricks-") }

// DropFromMenu removes ids from the global saved menu overlay and returns those it held; write false only reports.
func DropFromMenu(ids []string, write bool) (removed []string, err error) {
	menu, ok := vaultOpencodeMenu()
	if !ok {
		return nil, nil
	}
	want := idSet(ids)
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

// OpencodeMenuFor appends profile additions with known protocol flags to the unchanged legacy menu.
func OpencodeMenuFor(p *Provider) ([]OpencodeModel, error) {
	out := slices.Clone(OpencodeMenu())
	c, err := pickerRecord(p)
	if err != nil {
		return out, err
	}
	dropped := idSet(c.Dropped)
	out = slices.DeleteFunc(out, func(m OpencodeModel) bool { return dropped[MenuID(m.ID)] })
	saved := c.Models
	seen := make(map[string]bool)
	for _, m := range out {
		seen[p.WireModel(m.ID)] = true
	}
	for _, m := range saved {
		if seen[p.WireModel(models.Strip1M(m.ID))] {
			continue
		}
		out = append(out, OpencodeModel{ID: m.ID, Label: cmp.Or(m.Name, m.ID), Responses: models.ResponsesOnly(m.ID)})
		seen[p.WireModel(models.Strip1M(m.ID))] = true
	}
	return out, nil
}
