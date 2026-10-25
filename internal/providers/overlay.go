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

// UpdateOverlayDoc runs fn on the raw overlay/providers doc, returning it before and after; apply=false writes nothing.
// A write that changes an existing row's auth.authType is refused; use UpdateOverlayDocForce to allow it.
func UpdateOverlayDoc(apply bool, fn func(top map[string]json.RawMessage, tx *vault.Tx) error) (oldDoc, newDoc []byte, err error) {
	return UpdateOverlayDocForce(apply, false, fn)
}

// UpdateOverlayDocForce is UpdateOverlayDoc with the auth.authType guard lifted when force is set.
func UpdateOverlayDocForce(apply, force bool, fn func(top map[string]json.RawMessage, tx *vault.Tx) error) (oldDoc, newDoc []byte, err error) {
	run := vault.View
	if apply {
		run = vault.Update
	}
	err = run(func(tx *vault.Tx) error {
		old, _ := tx.Get(vault.Overlay, "providers")
		oldDoc = []byte(old)
		top := map[string]json.RawMessage{}
		if old != "" {
			if err := json.Unmarshal(oldDoc, &top); err != nil {
				return fmt.Errorf("vault overlay/providers: %w", err)
			}
		}
		if top == nil {
			top = map[string]json.RawMessage{}
		}
		if err := fn(top, tx); err != nil {
			return err
		}
		nd, err := json.MarshalIndent(top, "", "  ")
		if err != nil {
			return err
		}
		newDoc = append(nd, '\n')
		if err := Validate(newDoc); err != nil {
			return err
		}
		if !force {
			if err := checkAuthTypeChange(oldDoc, newDoc); err != nil {
				return err
			}
		}
		if !apply {
			return nil
		}
		if err := tx.Put(vault.Overlay, "providers.prev", old); err != nil {
			return err
		}
		return tx.Put(vault.Overlay, "providers", string(newDoc))
	})
	return oldDoc, newDoc, err
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

// overlayAuthTypes maps each overlay row to its auth.authType; rows without one map to "".
func overlayAuthTypes(doc []byte) map[string]string {
	var d struct {
		Providers map[string]struct {
			Auth struct {
				AuthType string `json:"authType"`
			} `json:"auth"`
		} `json:"providers"`
	}
	out := map[string]string{}
	if json.Unmarshal(doc, &d) != nil {
		return out
	}
	for n, p := range d.Providers {
		out[n] = p.Auth.AuthType
	}
	return out
}

// checkAuthTypeChange refuses a write that changes an existing row's auth.authType (add/drop of a row is fine).
func checkAuthTypeChange(oldDoc, newDoc []byte) error {
	oldK, newK := overlayAuthTypes(oldDoc), overlayAuthTypes(newDoc)
	for n, was := range oldK {
		if now, ok := newK[n]; ok && now != was {
			return fmt.Errorf("provider %s: auth.authType %q -> %q refused without force (changes how its credential is minted)", n, was, now)
		}
	}
	return nil
}
