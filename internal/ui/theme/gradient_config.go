package theme

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/edotau/claude-code/internal/paths"
)

// CurrentGradient is the palette the status line uses now: the user config's "gradient", else the default's.
func CurrentGradient() string {
	var cfg struct {
		Gradient string `json:"gradient"`
	}
	if raw, err := os.ReadFile(filepath.Join(StatuslineDir(), "config.json")); err == nil {
		if json.Unmarshal(raw, &cfg) == nil && cfg.Gradient != "" {
			return cfg.Gradient
		}
	}
	if err := json.Unmarshal(DefaultConfigJSON(), &cfg); err != nil {
		return "" // the embedded default must stay a valid JSON object; a stale cfg would mislead the caller
	}
	return cfg.Gradient
}

// SetGradient merges "gradient":name into state/statusline/config.json, creating it from the
// embedded default when absent. It never touches any other key, and never touches settings.json.
func SetGradient(name string) error {
	path := filepath.Join(StatuslineDir(), "config.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		raw = DefaultConfigJSON()
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	if m == nil { // "null" decodes without error but leaves m nil
		return fmt.Errorf("parse %s: not a JSON object", path)
	}
	encoded, err := json.Marshal(name)
	if err != nil {
		return err
	}
	m["gradient"] = encoded
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return paths.AtomicWrite(path, append(out, '\n'), 0o644)
}

// ResetConfig deletes state/statusline/config.json so the embedded default renders; false when there was none.
func ResetConfig() (bool, error) {
	err := os.Remove(filepath.Join(StatuslineDir(), "config.json"))
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}
