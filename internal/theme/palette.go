package theme

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/edotau/claude-code/internal/ansi"
)

// Palette is the shared look the TUI panels inherit: Border becomes Accent, Sel becomes Selection.
type Palette struct{ Accent, Selection ansi.RGB }

// LoadPalette reads <dir>/palette.json; alpha in #RRGGBBAA is ignored. ok=false if missing, invalid or a slot is absent.
func LoadPalette(dir string) (Palette, bool) {
	raw, err := os.ReadFile(filepath.Join(dir, "palette.json"))
	if err != nil {
		return Palette{}, false
	}
	var m map[string]string
	if json.Unmarshal(raw, &m) != nil {
		return Palette{}, false
	}
	accent, ok1 := slot(m, "Border")
	sel, ok2 := slot(m, "Sel")
	if !ok1 || !ok2 {
		return Palette{}, false
	}
	return Palette{Accent: accent, Selection: sel}, true
}

func slot(m map[string]string, key string) (ansi.RGB, bool) {
	v, ok := m[key]
	if !ok {
		return ansi.RGB{}, false
	}
	rgb, _, ok := ansi.ParseHex(v)
	return rgb, ok
}

var (
	currentOnce sync.Once
	currentPal  Palette
	currentOK   bool
)

// Current is LoadPalette(Dir()), resolved once per process.
func Current() (Palette, bool) {
	currentOnce.Do(func() { currentPal, currentOK = LoadPalette(Dir()) })
	return currentPal, currentOK
}
