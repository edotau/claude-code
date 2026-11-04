package providers

import (
	"cmp"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/vault"
)

// pickerCatalog stores profile-scoped selectable history, not current availability.
type pickerCatalog struct {
	Schema int         `json:"schema"`
	Scope  string      `json:"scope"`
	Models []ModelInfo `json:"models"`
}

var _ = schemas.Register(schemas.Format{
	Name: "picker-additions", Owner: "internal/providers", Kind: schemas.Store,
	Glob: vault.Path, Version: 1,
	Keep: "profile-scoped append-only sealed picker-additions/v1 records inside the vault; not live availability",
})

func pickerScope(p *Provider) string {
	// Hash configuration without exposing auth commands or headers; models/pins are not scope.
	data, _ := json.Marshal([]any{p.Name, p.Provider, p.Login, p.Identity,
		p.Protocol, p.BaseURL, p.Routes, p.Host, p.CLIProfile, p.SpellingRule(), p.Options,
		p.Auth, p.Headers})
	return fmt.Sprintf("%x", sha256.Sum256(data))
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

// PickerModels reads this profile's additions; only a missing record counts as empty.
func PickerModels(p *Provider) ([]ModelInfo, error) {
	raw, found, err := vault.ReadSecret(pickerKey(p))
	if err != nil {
		return nil, errors.New("cannot read or decrypt saved picker additions")
	}
	if !found {
		return nil, nil
	}
	c, err := decodePicker(raw, pickerScope(p))
	return c.Models, err
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

// pickerNew excludes configured aliases and the legacy menu using known wire equivalence.
func pickerNew(p *Provider, saved, live []ModelInfo) []ModelInfo {
	return pickerNewWithBaseline(p, saved, live, OpencodeMenu())
}

func pickerNewWithBaseline(p *Provider, saved, live []ModelInfo, baseline []OpencodeModel) []ModelInfo {
	seen := pickerSeen(p, saved, baseline)
	var out []ModelInfo
	for _, m := range live {
		if m.ID == "" || seen[p.WireModel(models.Strip1M(m.ID))] {
			continue
		}
		if p.Preset == PresetDatabricks && !m.Gateway && (m.Ready != "READY" || m.Task != "llm/v1/chat") {
			continue
		}
		seen[p.WireModel(models.Strip1M(m.ID))] = true
		// Do not persist readiness or capabilities as a claim of current availability.
		out = append(out, ModelInfo{ID: m.ID, Name: m.Name, UseCase: m.UseCase})
	}
	slices.SortFunc(out, func(a, b ModelInfo) int { return cmp.Compare(a.ID, b.ID) })
	return out
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
	saved, err := PickerModels(p)
	if err != nil {
		return nil, err
	}
	return pickerNew(p, saved, live), nil
}

// AppendPickerModels atomically appends sealed additions without losing concurrent updates.
func AppendPickerModels(p *Provider, live []ModelInfo) (added []ModelInfo, err error) {
	name, scope := pickerKey(p), pickerScope(p)
	baseline := OpencodeMenu()
	err = vault.UpdateSealed(func(tx *vault.Tx, key *vault.Key) error {
		added = nil // UpdateSealed may retry after a concurrent key rotation.
		c := pickerCatalog{Schema: 1, Scope: scope}
		record, found, err := tx.Record(name)
		if err != nil {
			return errors.New("cannot read saved picker additions record")
		}
		if found {
			raw, err := key.Open(name, record)
			if err != nil {
				return errors.New("cannot decrypt saved picker additions; refusing to overwrite")
			}
			c, err = decodePicker(raw, scope)
			if err != nil {
				return err
			}
		}
		// Use the captured baseline to avoid a nested vault view inside this transaction.
		added = pickerNewWithBaseline(p, c.Models, live, baseline)
		if len(added) == 0 {
			return nil
		}
		c.Models = MergePickerModels(c.Models, added)
		data, err := json.Marshal(c)
		if err != nil {
			return errors.New("cannot encode picker additions")
		}
		return tx.PutRecord(name, vault.Record{Sealed: key.Seal(name, string(data)), Kid: key.Kid,
			Len: len(data), Label: "picker additions", SetAt: time.Now().UTC()})
	})
	return added, err
}

// OpencodeMenuFor appends profile additions with known protocol flags to the unchanged legacy menu.
func OpencodeMenuFor(p *Provider) ([]OpencodeModel, error) {
	out := slices.Clone(OpencodeMenu())
	saved, err := PickerModels(p)
	if err != nil {
		return out, err
	}
	seen := make(map[string]bool)
	for _, m := range out {
		seen[p.WireModel(m.ID)] = true
	}
	for _, m := range saved {
		if seen[p.WireModel(models.Strip1M(m.ID))] {
			continue
		}
		label := m.Name
		if label == "" {
			label = m.ID
		}
		out = append(out, OpencodeModel{ID: m.ID, Label: label, Responses: models.ResponsesOnly(m.ID)})
		seen[p.WireModel(models.Strip1M(m.ID))] = true
	}
	return out, nil
}
