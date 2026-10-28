package providers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sort"
	"sync"
)

// ProviderKeys are the registry's provider keys, the parents of every profile.
var ProviderKeys = []string{"anthropic", "databricks", "github", "openai", "google", "nvidia"}

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

// RawOrNull is raw, or the JSON null literal when raw is nil.
func RawOrNull(raw json.RawMessage) json.RawMessage {
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
	if !slices.Contains(ProviderKeys, key) {
		return fmt.Errorf("unknown provider %q (known: %v)", key, ProviderKeys)
	}
	for n, p := range known {
		if p == key {
			out[n] = rawRow{provider: p}
		}
	}
	return nil
}

func addNested(out map[string]rawRow, key string, fields map[string]json.RawMessage) error {
	if !slices.Contains(ProviderKeys, key) && key != providerLocal {
		return fmt.Errorf("unknown provider %q (known: %v)", key, ProviderKeys)
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
			inner[n] = RawOrNull(rows[n].raw)
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
	rest := []string{}
	for n := range m {
		if !seen[n] {
			rest = append(rest, n)
		}
	}
	sort.Strings(rest)
	return append(out, rest...)
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
