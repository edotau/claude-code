package providers

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// SetProviderFields merges fields into providers.<name> of the user overlay at the raw-JSON level: never through
// Provider.MarshalJSON (it redacts headers). An entry the overlay lacks is seeded from the built-in one first,
// because an overlay entry replaces the built-in whole. Backs the old file up, then writes atomically.
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
	file := paths.ProvidersOverlay()
	return paths.WithFileLock(file, func() error {
		raw, err := os.ReadFile(file)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		doc := map[string]json.RawMessage{}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &doc); err != nil {
				return fmt.Errorf("%s: %w", file, err)
			}
		}
		provs := map[string]map[string]json.RawMessage{}
		if b, ok := doc["providers"]; ok {
			if err := json.Unmarshal(b, &provs); err != nil {
				return fmt.Errorf("%s: providers: %w", file, err)
			}
		}
		entry := provs[name]
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
		out, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			return err
		}
		if len(raw) > 0 {
			if err := paths.AtomicWrite(file+".bak-"+time.Now().Format("20060102-150405.000000000"), raw, 0o600); err != nil {
				return err
			}
		}
		return paths.AtomicWrite(file, append(out, '\n'), 0o600)
	})
}

// SetProviderModel sets one slot's model id for name, keeping its other slots.
func SetProviderModel(name, slot, id string) error {
	return updateProviderFields(name, func(entry map[string]json.RawMessage) error {
		reg, err := Load()
		if err != nil {
			return err
		}
		p, err := reg.Get(name)
		if err != nil {
			return err
		}
		m := p.Models
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
