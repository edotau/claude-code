package providers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sync"

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

// providerKeys are the registry's provider keys, the parents of every profile.
var providerKeys = []string{"anthropic", "databricks", "github", "openai", "google"}

// rawRow is one profile's raw JSON under its provider; raw == nil removes the built-in.
type rawRow struct {
	provider string
	raw      json.RawMessage
}

// builtinProviders maps every built-in profile to its provider.
var builtinProviders = sync.OnceValue(func() map[string]string {
	rows, err := decodeRows(providersField(defaultsJSON), nil)
	out := map[string]string{}
	if err != nil {
		return out
	}
	for n, r := range rows {
		out[n] = r.provider
	}
	return out
})

// providersField returns the raw "providers" value of a registry document.
func providersField(doc []byte) json.RawMessage {
	var top struct {
		Providers json.RawMessage `json:"providers"`
	}
	_ = json.Unmarshal(doc, &top)
	return top.Providers
}

// IsNull reports whether raw is the JSON null literal.
func IsNull(raw json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(raw), []byte("null")) }

// rawOrNull is raw, or the JSON null literal when raw is nil.
func rawOrNull(raw json.RawMessage) json.RawMessage {
	if raw == nil {
		return json.RawMessage("null")
	}
	return raw
}

// decodeRows reads a providers value, nested {provider: {profile: row}}; known maps built-in profiles to providers.
// It rejects a profile that appears under two providers.
func decodeRows(raw json.RawMessage, known map[string]string) (map[string]rawRow, error) {
	out := map[string]rawRow{}
	if len(raw) == 0 || IsNull(raw) {
		return out, nil
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, err
	}
	for _, key := range slices.Sorted(maps.Keys(top)) {
		entry := top[key]
		if IsNull(entry) {
			if err := removeEntry(out, key, known); err != nil {
				return nil, err
			}
			continue
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(entry, &fields); err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		if err := addNested(out, key, fields); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// removeEntry handles `"provider": null`: it drops that provider's built-in profiles.
func removeEntry(out map[string]rawRow, key string, known map[string]string) error {
	if !slices.Contains(providerKeys, key) {
		return fmt.Errorf("unknown provider %q (known: %v)", key, providerKeys)
	}
	for n, p := range known {
		if p == key {
			out[n] = rawRow{provider: p}
		}
	}
	return nil
}

func addNested(out map[string]rawRow, key string, fields map[string]json.RawMessage) error {
	if !slices.Contains(providerKeys, key) && key != providerLocal {
		return fmt.Errorf("unknown provider %q (known: %v)", key, providerKeys)
	}
	for profile, row := range fields {
		r := rawRow{provider: key}
		if !IsNull(row) {
			r.raw = row
		}
		if err := put(out, profile, r); err != nil {
			return err
		}
	}
	return nil
}

// put adds a row, rejecting a profile name already held by a different provider (names are unique registry-wide).
func put(out map[string]rawRow, profile string, r rawRow) error {
	if prev, ok := out[profile]; ok && prev.provider != r.provider && r.provider != "" && prev.provider != "" {
		return fmt.Errorf("profile %q is defined under both %s and %s; profile names are unique across providers",
			profile, prev.provider, r.provider)
	}
	out[profile] = r
	return nil
}

// nestRows renders rows as {provider: {profile: row}} in order (then sorted leftovers); a removal renders as null.
func nestRows(rows map[string]rawRow, order []string) (json.RawMessage, error) {
	names := orderedNames(order, func(n string) bool { _, ok := rows[n]; return ok }, rows)
	var provs []string
	groups := map[string][]string{}
	for _, n := range names {
		prov := rows[n].provider
		if prov == "" {
			prov = providerLocal
		}
		if groups[prov] == nil {
			provs = append(provs, prov)
		}
		groups[prov] = append(groups[prov], n)
	}
	outer := map[string]json.RawMessage{}
	for _, prov := range provs {
		inner := map[string]json.RawMessage{}
		for _, n := range groups[prov] {
			inner[n] = rawOrNull(rows[n].raw)
		}
		b, err := MarshalOrdered(groups[prov], inner)
		if err != nil {
			return nil, err
		}
		outer[prov] = b
	}
	return MarshalOrdered(provs, outer)
}

// orderedNames is order's entries that exist (deduped), then the remaining keys of m sorted.
func orderedNames[V any](order []string, has func(string) bool, m map[string]V) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range order {
		if has(n) && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	for _, n := range slices.Sorted(maps.Keys(m)) {
		if !seen[n] {
			out = append(out, n)
		}
	}
	return out
}

// MarshalOrdered renders m as a JSON object with keys in the given order (keys absent from m are skipped).
func MarshalOrdered(keys []string, m map[string]json.RawMessage) (json.RawMessage, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	first := true
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		if !first {
			buf.WriteByte(',')
		}
		first = false
		buf.Write(kb)
		buf.WriteByte(':')
		buf.Write(v)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// ObjectKeys lists a JSON object's keys in document order; a non-object yields none.
func ObjectKeys(raw json.RawMessage) []string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil
	}
	var keys []string
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			break
		}
		keys = append(keys, fmt.Sprint(k))
		var skip json.RawMessage
		if dec.Decode(&skip) != nil {
			break
		}
	}
	return keys
}

// profileOrder lists a providers value's profile names in document order.
func profileOrder(raw json.RawMessage) []string {
	var top map[string]json.RawMessage
	if json.Unmarshal(raw, &top) != nil {
		return nil
	}
	var out []string
	for _, key := range ObjectKeys(raw) {
		var fields map[string]json.RawMessage
		if IsNull(top[key]) || json.Unmarshal(top[key], &fields) != nil {
			continue
		}
		out = append(out, ObjectKeys(top[key])...)
	}
	return out
}

// declaredOrder is the defaults' profile order then the overlay's new names, limited to rows still registered.
func declaredOrder(reg *Registry, base, overlay []byte) []string {
	var out []string
	seen := map[string]bool{}
	for _, doc := range [][]byte{base, overlay} {
		for _, n := range profileOrder(providersField(doc)) {
			if _, ok := reg.Providers[n]; ok && !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	return out
}
