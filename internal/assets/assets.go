// Package assets owns every file embedded in the claude-code binary.
package assets

import "embed"

//go:embed files
var files embed.FS

// read panics on a missing name (a build defect); ReadFile already returns a fresh copy of the bytes.
func read(name string) []byte {
	b, err := files.ReadFile(name)
	if err != nil {
		panic("assets: " + name + ": " + err.Error())
	}
	return b
}

// RankTable is the model rank table (tiers, rotation rules, families).
func RankTable() []byte { return read("files/catalog/ranks.json") }

// RetiredWords is the retired-word vocabulary the source review advises on.
func RetiredWords() []byte { return read("files/review/source/retired_words.json") }

// ThemeDefaultConfig is the statusline config.json default.
func ThemeDefaultConfig() []byte { return read("files/ui/theme/default_config.json") }

// ThemeDefaultPalettes is the statusline palette default.
func ThemeDefaultPalettes() []byte { return read("files/ui/theme/default_palettes.json") }
