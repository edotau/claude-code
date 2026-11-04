package providers

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/edotau/claude-code/internal/vault"
)

// SetProviderFields merges fields into profile name of the user overlay at the raw-JSON level: never through
// Provider.MarshalJSON (it redacts headers). An entry the overlay lacks is seeded from the built-in one first,
// because an overlay entry replaces the built-in whole. Keeps the previous generation as overlay/providers.prev.
func SetProviderFields(name string, fields map[string]any) error {
	return updateProviderFields(name, func(entry map[string]json.RawMessage) error {
		for k, v := range fields {
			b, err := json.Marshal(v)
			if err != nil {
				return err
			}
			entry[k] = b
		}
		return nil
	})
}

// UpdateOverlayDoc runs fn on the raw overlay/providers doc, returning it before and after; apply=false writes nothing.
// fn sees top["providers"] as flat {profile: row}; the stored document is always nested by provider.
// A write that changes an existing row's sealed auth.type is refused; use UpdateOverlayDocForce to allow it.
func UpdateOverlayDoc(apply bool, fn func(top map[string]json.RawMessage, tx *vault.Tx) error) (oldDoc, newDoc []byte, err error) {
	return UpdateOverlayDocForce(apply, false, fn)
}

// UpdateOverlayDocForce is UpdateOverlayDoc with the auth.type guard lifted when force is set.
func UpdateOverlayDocForce(apply, force bool, fn func(top map[string]json.RawMessage, tx *vault.Tx) error) (oldDoc, newDoc []byte, err error) {
	run := vault.View
	if apply {
		run = vault.Update
	}
	err = run(func(tx *vault.Tx) error {
		old, _ := tx.Get(vault.Overlay, "providers")
		var err error
		oldDoc, newDoc, err = overlayTx(tx, old, apply, force, fn)
		return err
	})
	return oldDoc, newDoc, err
}

// overlayTx is one UpdateOverlayDocForce transaction over the stored doc old: decode, edit, validate, optionally store.
func overlayTx(tx *vault.Tx, old string, apply, force bool,
	fn func(top map[string]json.RawMessage, tx *vault.Tx) error) (oldDoc, newDoc []byte, err error) {
	top, err := decodeOverlayTop(old)
	if err != nil {
		return nil, nil, err
	}
	if oldDoc, err = nestedDoc(top); err != nil {
		return oldDoc, nil, err
	}
	flat, owner, err := flattenTop(top)
	if err != nil {
		return oldDoc, nil, err
	}
	order := profileOrder(top["providers"])
	if err := fn(flat, tx); err != nil {
		return oldDoc, nil, err
	}
	if newDoc, err = renderNested(flat, owner, order); err != nil {
		return oldDoc, newDoc, err
	}
	if err := checkOverlayDoc(oldDoc, newDoc, force); err != nil {
		return oldDoc, newDoc, err
	}
	if !apply {
		return oldDoc, newDoc, nil
	}
	if err := tx.Put(vault.Overlay, "providers.prev", old); err != nil {
		return oldDoc, newDoc, err
	}
	return oldDoc, newDoc, tx.Put(vault.Overlay, "providers", string(newDoc))
}

// decodeOverlayTop decodes the stored overlay doc into its top-level raw fields; empty gives an empty map.
func decodeOverlayTop(old string) (map[string]json.RawMessage, error) {
	top := map[string]json.RawMessage{}
	if old != "" {
		if err := json.Unmarshal([]byte(old), &top); err != nil {
			return nil, fmt.Errorf("vault overlay/providers: %w", err)
		}
	}
	if top == nil {
		top = map[string]json.RawMessage{}
	}
	return top, nil
}

// checkOverlayDoc validates the new doc and, unless force is set, refuses a sealed auth.type change.
func checkOverlayDoc(oldDoc, newDoc []byte, force bool) error {
	if err := Validate(newDoc); err != nil {
		return err
	}
	if force {
		return nil
	}
	return checkAuthTypeChange(oldDoc, newDoc)
}

// flattenTop returns a copy of top whose providers value is the flat {profile: row} view, plus each profile's provider.
func flattenTop(top map[string]json.RawMessage) (map[string]json.RawMessage, map[string]string, error) {
	flat := maps.Clone(top)
	rows, err := decodeRows(top["providers"], builtinProviders())
	if err != nil {
		return nil, nil, fmt.Errorf("vault overlay/providers: providers: %w", err)
	}
	view := map[string]json.RawMessage{}
	owner := map[string]string{}
	for n, r := range rows {
		owner[n] = r.provider
		if r.raw == nil {
			view[n] = json.RawMessage("null")
			continue
		}
		view[n] = r.raw
	}
	if _, ok := top["providers"]; ok || len(view) > 0 {
		order := orderedNames(profileOrder(top["providers"]), func(n string) bool { _, ok := view[n]; return ok }, view)
		b, err := MarshalOrdered(order, view)
		if err != nil {
			return nil, nil, err
		}
		flat["providers"] = b
	}
	return flat, owner, nil
}

// renderNested re-nests flat's providers by provider (owner, else inferred), keeping order, then flat's own key order.
func renderNested(flat map[string]json.RawMessage, owner map[string]string, order []string) ([]byte, error) {
	top := maps.Clone(flat)
	if raw, ok := flat["providers"]; ok {
		var view map[string]json.RawMessage
		if err := json.Unmarshal(raw, &view); err != nil {
			return nil, fmt.Errorf("vault overlay/providers: providers: %w", err)
		}
		rows := make(map[string]rawRow, len(view))
		for n, row := range view {
			r := rawRow{provider: owner[n], raw: row}
			if IsNull(row) {
				r.raw = nil
			} else if r.provider == "" {
				r.provider = inferProvider(n, row)
			}
			rows[n] = r
		}
		nested, err := nestRows(rows, append(slices.Clone(order), ObjectKeys(raw)...))
		if err != nil {
			return nil, err
		}
		top["providers"] = nested
	}
	nd, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(nd, '\n'), nil
}

// nestedDoc is the document with its providers value nested; a document that does not parse is returned as is.
func nestedDoc(top map[string]json.RawMessage) ([]byte, error) {
	flat, owner, err := flattenTop(top)
	if err != nil {
		return nil, err
	}
	return renderNested(flat, owner, profileOrder(top["providers"]))
}

// inferProvider files a new row without an owner under its own provider field, else databricks (preset or sealed auth).
func inferProvider(name string, row json.RawMessage) string {
	if prov, ok := builtinProviders()[name]; ok {
		return prov
	}
	var p Provider
	if json.Unmarshal(row, &p) != nil {
		return providerLocal
	}
	if p.Provider != "" {
		return p.Provider
	}
	if p.Preset == PresetDatabricks || sealedAuth(p.Auth.Type) {
		return GroupDatabricks
	}
	return providerLocal
}

func updateProviderFields(name string, update func(map[string]json.RawMessage) error) error {
	_, _, err := UpdateOverlayDoc(true, func(doc map[string]json.RawMessage, _ *vault.Tx) error {
		provs := map[string]map[string]json.RawMessage{}
		if b, ok := doc["providers"]; ok {
			if err := json.Unmarshal(b, &provs); err != nil {
				return fmt.Errorf("vault overlay/providers: providers: %w", err)
			}
		}
		if provs == nil {
			provs = map[string]map[string]json.RawMessage{}
		}
		entry := provs[name]
		var err error
		if entry == nil {
			if entry, err = builtinEntry(name); err != nil {
				return err
			}
		}
		if err := update(entry); err != nil {
			return err
		}
		provs[name] = entry
		doc["providers"], err = json.Marshal(provs)
		return err
	})
	return err
}

// SetProviderModel sets one alias's model id for name, keeping its other aliases.
func SetProviderModel(name, alias, id string) error {
	return updateProviderFields(name, func(entry map[string]json.RawMessage) error {
		var m map[string]string
		if raw, ok := entry["models"]; ok {
			if err := json.Unmarshal(raw, &m); err != nil {
				return fmt.Errorf("vault overlay/providers: models: %w", err)
			}
		}
		if m == nil {
			m = map[string]string{}
		}
		m[alias] = id
		raw, err := json.Marshal(m)
		if err != nil {
			return err
		}
		entry["models"] = raw
		return nil
	})
}

// builtinEntry is defaults.json's raw entry for profile name (empty when there is none).
func builtinEntry(name string) (map[string]json.RawMessage, error) {
	rows, err := decodeRows(providersField(defaultsJSON), nil)
	if err != nil {
		return nil, err
	}
	out := map[string]json.RawMessage{}
	if r, ok := rows[name]; ok && r.raw != nil {
		if err := json.Unmarshal(r.raw, &out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// overlayAuthTypes maps every overlay row to its sealed auth type (oauth, m2m, pat), or "" when it has none.
func overlayAuthTypes(doc []byte) (map[string]string, error) {
	out := map[string]string{}
	reg, err := decodeRegistry(doc, builtinProviders())
	if err != nil {
		return nil, err
	}
	for n, p := range reg.Providers {
		if p == nil {
			continue
		}
		out[n] = ""
		if sealedAuth(p.Auth.Type) {
			out[n] = p.Auth.Type
		}
	}
	return out, nil
}

// checkAuthTypeChange refuses a write that changes an existing row's sealed auth.type (add/drop of a row is fine).
func checkAuthTypeChange(oldDoc, newDoc []byte) error {
	oldK, err := overlayAuthTypes(oldDoc)
	if err != nil { // fail closed: an undecodable current overlay hides any auth.type change
		return fmt.Errorf("current provider overlay does not decode (%w); auth.type changes refused without force", err)
	}
	newK, err := overlayAuthTypes(newDoc)
	if err != nil {
		return err
	}
	for n, was := range oldK {
		if now, ok := newK[n]; ok && now != was {
			return fmt.Errorf("provider %s: auth.type %q -> %q refused without force (changes how its credential is minted)", n, was, now)
		}
	}
	return nil
}
