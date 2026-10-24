package settings

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/edotau/claude-code/internal/vault"
)

// overlayBucketKey is where the per-machine settings page overrides live: vault bucket
// Overlay, plain (not sealed), one JSON object of catalog key -> value.
const overlayBucketKey = "settings"

// dropBucketKey is the vault bucket Overlay entry holding top-level settings.json keys removed on every render.
const dropBucketKey = "settings-drop"

// LoadDropped reads the drop list; an absent or empty entry is nil.
func LoadDropped() ([]string, error) {
	var keys []string
	err := vault.View(func(t *vault.Tx) error {
		raw, ok := t.Get(vault.Overlay, dropBucketKey)
		if !ok || raw == "" {
			return nil
		}
		return json.Unmarshal([]byte(raw), &keys)
	})
	if err != nil {
		return nil, fmt.Errorf("vault overlay/%s: %w", dropBucketKey, err)
	}
	return keys, nil
}

// CheckDroppable refuses any key the template owns (catalogued or rendered at top level): a render puts it back.
func CheckDroppable(keys []string) error {
	rendered, err := Render()
	if err != nil {
		return err
	}
	tp, err := parseObject(rendered)
	if err != nil {
		return err
	}
	for _, k := range keys {
		_, catalogued := Lookup(k)
		if _, rendered := tp.vals[k]; catalogued || rendered {
			return fmt.Errorf("%s is owned by the template; edit internal/settings/template/", k)
		}
	}
	return nil
}

// UpdateDropped applies next to the stored list inside one vault transaction (sorted, deduped, checked);
// an empty result deletes the entry instead of storing "[]".
func UpdateDropped(next func(cur []string) []string) error {
	return vault.Update(func(t *vault.Tx) error {
		var cur []string
		if raw, ok := t.Get(vault.Overlay, dropBucketKey); ok && raw != "" {
			if err := json.Unmarshal([]byte(raw), &cur); err != nil {
				return fmt.Errorf("vault overlay/%s: %w", dropBucketKey, err)
			}
		}
		keys := slices.Compact(slices.Sorted(slices.Values(next(cur))))
		if err := CheckDroppable(keys); err != nil {
			return err
		}
		var raw string
		if len(keys) > 0 {
			b, err := json.Marshal(keys)
			if err != nil {
				return err
			}
			raw = string(b)
		}
		return t.Put(vault.Overlay, dropBucketKey, raw)
	})
}

// SaveDropped replaces the stored list with keys.
func SaveDropped(keys []string) error {
	return UpdateDropped(func([]string) []string { return keys })
}

// LoadOverlay reads the vault overlay/settings bucket; an absent entry is an empty map, not an error.
func LoadOverlay() (map[string]json.RawMessage, error) {
	ov := map[string]json.RawMessage{}
	err := vault.View(func(t *vault.Tx) error {
		raw, ok := t.Get(vault.Overlay, overlayBucketKey)
		if !ok || raw == "" {
			return nil
		}
		return json.Unmarshal([]byte(raw), &ov)
	})
	if err != nil {
		return nil, fmt.Errorf("vault overlay/settings: %w", err)
	}
	return ov, nil
}

// ValidateOverlay checks every key against Catalog and runs its Validate.
func ValidateOverlay(ov map[string]json.RawMessage) error {
	for k, v := range ov {
		f, ok := Lookup(k)
		if !ok {
			return fmt.Errorf("vault overlay/settings: %s: unknown key", k)
		}
		if err := f.Validate(v); err != nil {
			return fmt.Errorf("vault overlay/settings: %s: %w", k, err)
		}
	}
	return nil
}

// SaveOverlay validates every key against Catalog, then writes ov whole; an empty map deletes
// the vault entry instead of storing "{}".
func SaveOverlay(ov map[string]json.RawMessage) error {
	if err := ValidateOverlay(ov); err != nil {
		return err
	}
	var raw string
	if len(ov) > 0 {
		b, err := json.Marshal(ov)
		if err != nil {
			return err
		}
		raw = string(b)
	}
	return vault.Update(func(t *vault.Tx) error {
		return t.Put(vault.Overlay, overlayBucketKey, raw)
	})
}

// RenderLive is Render with the vault overlay applied on top.
func RenderLive() ([]byte, error) {
	rendered, err := Render()
	if err != nil {
		return nil, err
	}
	ov, err := LoadOverlay()
	if err != nil {
		return nil, err
	}
	return applyOverlay(rendered, ov)
}

// applyOverlay is pure: it never touches the vault, so tests can drive it with fixed inputs.
// An unknown key or a value Validate rejects is an error naming the key and the reason.
func applyOverlay(rendered []byte, ov map[string]json.RawMessage) ([]byte, error) {
	o, err := parseObject(rendered)
	if err != nil {
		return nil, err
	}
	for k, v := range ov {
		f, ok := Lookup(k)
		if !ok {
			return nil, fmt.Errorf("vault overlay/settings: %s: unknown key", k)
		}
		if err := f.Validate(v); err != nil {
			return nil, fmt.Errorf("vault overlay/settings: %s: %w", k, err)
		}
		o.set(k, v)
	}
	return o.marshal()
}
