package providers

import (
	"encoding/json"
	"fmt"

	"github.com/edotau/claude-code/internal/vault"
)

// SetProviderFields merges fields into providers.<name> of the user overlay at the raw-JSON level: never through
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

func updateProviderFields(name string, update func(map[string]json.RawMessage) error) error {
	return vault.Update(func(tx *vault.Tx) error {
		old, _ := tx.Get(vault.Overlay, "providers")
		doc := map[string]json.RawMessage{}
		if len(old) > 0 {
			if err := json.Unmarshal([]byte(old), &doc); err != nil {
				return fmt.Errorf("vault overlay/providers: %w", err)
			}
		}
		if doc == nil {
			doc = map[string]json.RawMessage{}
		}
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
		if doc["providers"], err = json.Marshal(provs); err != nil {
			return err
		}
		newDoc, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			return err
		}
		newDoc = append(newDoc, '\n')
		if err := Validate(newDoc); err != nil {
			return err
		}
		if err := tx.Put(vault.Overlay, "providers.prev", old); err != nil {
			return err
		}
		return tx.Put(vault.Overlay, "providers", string(newDoc))
	})
}

// SetProviderModel sets one slot's model id for name, keeping its other slots.
func SetProviderModel(name, slot, id string) error {
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
		m[slot] = id
		raw, err := json.Marshal(m)
		if err != nil {
			return err
		}
		entry["models"] = raw
		return nil
	})
}

// builtinEntry is defaults.json's raw entry for name (empty when there is none).
func builtinEntry(name string) (map[string]json.RawMessage, error) {
	var def struct {
		Providers map[string]map[string]json.RawMessage `json:"providers"`
	}
	if err := json.Unmarshal(defaultsJSON, &def); err != nil {
		return nil, err
	}
	if e := def.Providers[name]; e != nil {
		return e, nil
	}
	return map[string]json.RawMessage{}, nil
}
