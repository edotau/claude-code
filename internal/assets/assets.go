// Package assets owns every file embedded in the claude-code binary.
package assets

import "embed"

//go:embed files
var files embed.FS

func read(name string) []byte {
	b, err := files.ReadFile(name)
	if err != nil {
		panic("assets: " + name + ": " + err.Error())
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

// RankTable is the model rank table (tiers, rotation rules, families).
func RankTable() []byte { return read("files/catalog/ranks.json") }

// ThemeDefaultConfig is the statusline config.json default.
func ThemeDefaultConfig() []byte { return read("files/ui/theme/default_config.json") }

// ThemeDefaultPalettes is the statusline palette default.
func ThemeDefaultPalettes() []byte { return read("files/ui/theme/default_palettes.json") }
