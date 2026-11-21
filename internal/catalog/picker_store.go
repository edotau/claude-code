package catalog

import (
	"cmp"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/vault"
)

// pickerCatalog stores profile-scoped selectable history, not current availability.
type pickerCatalog struct {
	Schema int                   `json:"schema"`
	Scope  string                `json:"scope"`
	Models []providers.ModelInfo `json:"models"`
	// Dropped ids are never re-added by discovery until `models discover --undrop`.
	Dropped []string `json:"dropped,omitempty"`
	// DroppedWhy maps a dropped id to "deprecated <date>"; absent for a hand drop.
	DroppedWhy map[string]string `json:"dropped_why,omitempty"`
}

var _ = schemas.Register(schemas.Format{
	Name: "picker-additions", Owner: "internal/catalog", Kind: schemas.Store,
	Glob: vault.Path, Version: 1,
	Keep: "profile-scoped sealed picker-additions/v1 records inside the vault (additions, dropped ids); not live availability",
})

func pickerScope(p *providers.Provider) string {
	// Hash configuration without exposing auth commands or headers; models/pins are not scope.
	opts := p.Options
	opts.WarehouseID, opts.Prices = "", nil // cost settings are not routing: setting them must not orphan the drop list
	routes := p.Routes
	if host := providers.PresetHost(p.Host); p.Preset == providers.PresetDatabricks && host != "" && routes[providers.ProtocolGemini] == host+"/serving-endpoints/gemini" {
		routes = maps.Clone(routes)
		delete(routes, providers.ProtocolGemini) // preset-derived default: omitting it preserves existing sealed drop hashes
	}
	data, _ := json.Marshal([]any{p.Name, p.Provider, p.Login, p.Identity,
		p.Protocol, p.BaseURL, routes, p.Host, p.CLIProfile, p.SpellingRule(), opts,
		p.Auth, p.Headers})
	return textutil.SHA256Hex(data)
}

func pickerKey(p *providers.Provider) string { return "picker-additions/v1/" + pickerScope(p) }

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
func pickerRecord(p *providers.Provider) (pickerCatalog, error) {
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
func PickerModels(p *providers.Provider) ([]providers.ModelInfo, error) {
	c, err := pickerRecord(p)
	return c.Models, err
}

// PickerDropped reads the ids dropped from this profile's picker.
func PickerDropped(p *providers.Provider) ([]string, error) {
	c, err := pickerRecord(p)
	return c.Dropped, err
}

// pickerNewWithBaseline excludes configured aliases and the baseline menu using known wire equivalence.
func pickerNewWithBaseline(p *providers.Provider, saved, live []providers.ModelInfo, baseline []providers.OpencodeModel, dropped []string) []providers.ModelInfo {
	seen := pickerSeen(p, saved, baseline)
	drop := providers.MenuIDSet(dropped)
	var out []providers.ModelInfo
	for _, m := range live {
		if m.ID == "" || drop[providers.MenuID(m.ID)] || seen[p.WireModel(models.Strip1M(m.ID))] {
			continue
		}
		if p.Preset == providers.PresetDatabricks && (!m.ReadyChat() || llama3Instruct(m.ID)) {
			continue
		}
		seen[p.WireModel(models.Strip1M(m.ID))] = true
		// Do not persist readiness or capabilities as a claim of current availability.
		out = append(out, providers.ModelInfo{ID: m.ID, Name: m.Name, UseCase: m.UseCase})
	}
	slices.SortFunc(out, func(a, b providers.ModelInfo) int { return cmp.Compare(a.ID, b.ID) })
	return out
}

// llama3Instruct is a llama 3.x instruct id, kept out of the catalog because it answers tool tasks wrong.
func llama3Instruct(id string) bool {
	id = strings.TrimPrefix(strings.ToLower(id), "databricks-")
	return strings.HasPrefix(id, "meta-llama-3-") && strings.HasSuffix(id, "-instruct")
}

// pickerSeen is the set of wire ids already offered: configured aliases, the legacy menu (Databricks), saved additions.
func pickerSeen(p *providers.Provider, saved []providers.ModelInfo, baseline []providers.OpencodeModel) map[string]bool {
	seen := make(map[string]bool)
	for _, id := range p.Models {
		seen[p.WireModel(models.Strip1M(id))] = true
	}
	if p.Preset == providers.PresetDatabricks {
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
func PreviewPickerModels(p *providers.Provider, live []providers.ModelInfo) ([]providers.ModelInfo, error) {
	c, err := pickerRecord(p)
	if err != nil {
		return nil, err
	}
	return pickerNewWithBaseline(p, c.Models, live, providers.OpencodeMenu(), c.Dropped), nil
}

// AppendPickerModels atomically appends sealed additions without losing concurrent updates.
func AppendPickerModels(p *providers.Provider, live []providers.ModelInfo) (added []providers.ModelInfo, err error) {
	name, scope := pickerKey(p), pickerScope(p)
	baseline := providers.OpencodeMenu()
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
		c.Models = providers.MergePickerModels(c.Models, added)
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

// applyDrop removes ids from c's additions and records them dropped; undrop clears the record instead.
func applyDrop(p *providers.Provider, c pickerCatalog, ids []string, undrop bool) (next pickerCatalog, removed []string) {
	want := providers.MenuIDSet(ids)
	next = c
	next.Schema, next.Scope = 1, pickerScope(p)
	if undrop {
		next.Dropped = slices.DeleteFunc(slices.Clone(c.Dropped), func(id string) bool { return want[providers.MenuID(id)] })
		next.DroppedWhy = maps.Clone(c.DroppedWhy)
		maps.DeleteFunc(next.DroppedWhy, func(id, _ string) bool { return want[providers.MenuID(id)] })
		return next, nil
	}
	next.Models = nil
	for _, m := range c.Models {
		if want[providers.MenuID(m.ID)] {
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
func EditPickerDrops(p *providers.Provider, ids []string, undrop, write bool) (removed []string, err error) {
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
		if unchanged || (!undrop && len(removed) == 0 && p.Preset != providers.PresetDatabricks) {
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
func RecordDropped(p *providers.Provider, id, reason string, at time.Time) (added bool, err error) {
	name, scope := pickerKey(p), pickerScope(p)
	err = vault.UpdateSealed(func(tx *vault.Tx, key *vault.Key) error {
		added = false // UpdateSealed may retry
		c, err := txPicker(tx, key, name, scope)
		if err != nil || providers.MenuIDSet(c.Dropped)[providers.MenuID(id)] {
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
func DroppedReasons(p *providers.Provider) (map[string]string, error) {
	c, err := pickerRecord(p)
	return c.DroppedWhy, err
}

// DroppedLive is the dropped ids of p that the live listing still serves, for discovery to report as skipped.
func DroppedLive(p *providers.Provider, live []providers.ModelInfo) ([]string, error) {
	c, err := pickerRecord(p)
	if err != nil || len(c.Dropped) == 0 {
		return nil, err
	}
	drop := providers.MenuIDSet(c.Dropped)
	var out []string
	for _, m := range live {
		if drop[providers.MenuID(m.ID)] {
			out = append(out, m.ID)
		}
	}
	return out, nil
}

// OpencodeMenuFor appends profile additions with known protocol flags to the unchanged legacy menu.
func OpencodeMenuFor(p *providers.Provider) ([]providers.OpencodeModel, error) {
	out := slices.Clone(providers.OpencodeMenu())
	c, err := pickerRecord(p)
	if err != nil {
		return out, err
	}
	dropped := providers.MenuIDSet(c.Dropped)
	out = slices.DeleteFunc(out, func(m providers.OpencodeModel) bool { return dropped[providers.MenuID(m.ID)] })
	saved := c.Models
	seen := make(map[string]bool)
	for _, m := range out {
		seen[p.WireModel(m.ID)] = true
	}
	for _, m := range saved {
		if seen[p.WireModel(models.Strip1M(m.ID))] {
			continue
		}
		out = append(out, providers.OpencodeModel{ID: m.ID, Label: cmp.Or(m.Name, m.ID), Responses: models.ResponsesOnly(m.ID)})
		seen[p.WireModel(models.Strip1M(m.ID))] = true
	}
	return out, nil
}
