// claude_settings.go — the "claude code" page: your defaults, grouped by Sections(), edited and held until
// the page is left, then saved as default to the vault overlay; it can also add any schema key or write repo defaults.
package configure

import (
	"encoding/json"
	"os"

	"github.com/edotau/claude-code/internal/launch"
)

// RenderObject parses render's output into a top-level key -> value map.
func RenderObject(render func() ([]byte, error)) (map[string]json.RawMessage, error) {
	raw, err := render()
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// ReadLiveSettings parses the live settings.json; a missing file is not an error (just no rows to flag).
func ReadLiveSettings() (map[string]json.RawMessage, error) {
	return RenderObject(func() ([]byte, error) { return os.ReadFile(launch.SettingsFile()) })
}
