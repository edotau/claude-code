package settings

import (
	"encoding/json"
	"fmt"

	"github.com/edotau/claude-code/internal/vault"
)

// overlayBucketKey is where the per-machine settings page overrides live: vault bucket
// Overlay, plain (not sealed), one JSON object of catalog key -> value.
const overlayBucketKey = "settings"

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

// SaveOverlay validates every key against Catalog, then writes ov whole; an empty map deletes
// the vault entry instead of storing "{}".
func SaveOverlay(ov map[string]json.RawMessage) error {
	for k, v := range ov {
		f, ok := Lookup(k)
		if !ok {
			return fmt.Errorf("vault overlay/settings: %s: unknown key", k)
		}
		if err := f.Validate(v); err != nil {
			return fmt.Errorf("vault overlay/settings: %s: %w", k, err)
		}
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
