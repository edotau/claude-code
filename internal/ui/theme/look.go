package theme

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/ui/ansi"
)

//go:embed default_config.json
var embeddedConfigJSON []byte

//go:embed default_palettes.json
var embeddedPalettesJSON []byte

// Look is the colour half of the statusline config.json: it decides how every claude-code surface is painted.
type Look struct {
	ColorMode string  `json:"colorMode"` // auto|truecolor|ansi256|ansi16
	Gradient  string  `json:"gradient"`  // named ramp in palettes.gradients
	Lift      float64 `json:"lift"`      // brighten every tone toward white: 0 = off, clamped to 0..0.6
}

// Palettes holds named gradient ramps, powerline themes (theme → key → {bg,fg}) and semantic roles.
type Palettes struct {
	Gradients map[string][]string                     `json:"gradients"`
	Powerline map[string]map[string]map[string]string `json:"powerline"`
	Semantic  map[string]string                       `json:"semantic"`
}

// Decoded on first use: this package links into every claude-code exec, only a few verbs read them.
var embeddedPalettes = sync.OnceValue(func() Palettes {
	var p Palettes
	_ = json.Unmarshal(embeddedPalettesJSON, &p)
	return p
})

// bakedSemantic covers roles a palette omits entirely.
var bakedSemantic = map[string]string{
	"dim": "#7C6C7C", "user": "#5AF78E", "punct": "#C8CAD6",
	"add": "#7ED37F", "del": "#E96969", "warn": "#B26A00", "danger": "#C2185B",
}

// Lifted brightens rgb toward white by the clamped lift: c + lift*(255-c) per channel.
func (l Look) Lifted(rgb ansi.RGB) ansi.RGB {
	f := math.Min(math.Max(l.Lift, 0), 0.6)
	if f == 0 {
		return rgb
	}
	for i, v := range rgb {
		rgb[i] = int(math.Round(float64(v) + f*float64(255-v)))
	}
	return rgb
}

// LiftPalette returns pal with every semantic tone (baked roles included) lifted; one pass per render.
func (l Look) LiftPalette(pal Palettes) Palettes {
	if l.Lift <= 0 {
		return pal
	}
	sem := make(map[string]string, len(bakedSemantic)+len(pal.Semantic))
	for role := range bakedSemantic {
		sem[role] = pal.Sem(role)
	}
	for role := range pal.Semantic {
		sem[role] = pal.Sem(role)
	}
	for role, h := range sem {
		r := l.Lifted(ansi.Hex(h))
		sem[role] = fmt.Sprintf("#%02X%02X%02X", r[0], r[1], r[2])
	}
	pal.Semantic = sem
	return pal
}

// Mode honors an explicit colorMode, else sniffs COLORTERM (truecolor when advertised, else 256).
func (l Look) Mode() string {
	if m := l.ColorMode; m != "" && m != "auto" {
		return m
	}
	ct := strings.ToLower(os.Getenv("COLORTERM"))
	if strings.Contains(ct, "truecolor") || strings.Contains(ct, "24bit") {
		return ansi.TrueColor
	}
	return ansi.ANSI256
}

// Stops returns the configured ramp's stops from pal, else the embedded gemini ramp, lifted.
func (l Look) Stops(pal Palettes) []ansi.RGB {
	hexes := pal.Gradients[l.Gradient]
	if len(hexes) == 0 {
		hexes = embeddedPalettes().Gradients["gemini"]
	}
	out := HexStops(hexes)
	for i, rgb := range out {
		out[i] = l.Lifted(rgb)
	}
	return out
}

// HexStops converts a palette's hex ramp to RGB stops.
func HexStops(hexes []string) []ansi.RGB {
	out := make([]ansi.RGB, len(hexes))
	for i, h := range hexes {
		out[i] = ansi.Hex(h)
	}
	return out
}

// Sem returns the hex for a semantic role, else its baked tone.
func (p Palettes) Sem(role string) string {
	if h := p.Semantic[role]; h != "" {
		return h
	}
	return bakedSemantic[role]
}

// EmbeddedPalettes returns the built-in palettes, ignoring local overrides.
func EmbeddedPalettes() Palettes { return embeddedPalettes() }

// StatuslineDir holds the optional config.json / palettes.json overrides.
func StatuslineDir() string { return filepath.Join(paths.ConfigDir(), "state", "statusline") }

// DefaultConfigJSON returns the embedded statusline/config.json default, for callers that must seed an absent file.
func DefaultConfigJSON() []byte { return embeddedConfigJSON }

// LoadPalettes returns palettes.json beside config.json, else the embedded palette.
func LoadPalettes() Palettes {
	data, err := os.ReadFile(filepath.Join(StatuslineDir(), "palettes.json"))
	if err != nil {
		return embeddedPalettes()
	}
	var pal Palettes
	if json.Unmarshal(data, &pal) != nil {
		return embeddedPalettes()
	}
	for role, h := range pal.Semantic {
		if !paintableHex(h) {
			delete(pal.Semantic, role) // Sem then serves the baked tone, not a silent black
		}
	}
	return pal
}

func paintableHex(h string) bool {
	n := len(strings.TrimPrefix(h, "#"))
	_, _, ok := ansi.ParseHex(h)
	return ok && (n == 3 || n == 6)
}

// GradientNames returns every named gradient ramp the resolved palette carries, sorted.
func GradientNames() []string {
	pal := LoadPalettes()
	names := make([]string, 0, len(pal.Gradients))
	for n := range pal.Gradients {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// EmbeddedTheme returns name's stops and the dim tone from the built-in palettes, ignoring local overrides.
func EmbeddedTheme(name string) (stops []ansi.RGB, dim ansi.RGB) {
	pal := embeddedPalettes()
	return HexStops(pal.Gradients[name]), ansi.Hex(pal.Sem("dim"))
}

// GradientStops returns name's RGB stops from the resolved palette, or nil if name is unknown.
func GradientStops(name string) []ansi.RGB {
	hexes, ok := LoadPalettes().Gradients[name]
	if !ok {
		return nil
	}
	return HexStops(hexes)
}

// Theme exposes the status line's color mode, gradient stops and dim tone so other output can match it.
func Theme() (mode string, stops []ansi.RGB, dim ansi.RGB) {
	var l Look
	_ = json.Unmarshal(embeddedConfigJSON, &l)
	if data, err := os.ReadFile(filepath.Join(StatuslineDir(), "config.json")); err == nil {
		_ = json.Unmarshal(data, &l)
	}
	pal := l.LiftPalette(LoadPalettes())
	return l.Mode(), l.Stops(pal), ansi.Hex(pal.Sem("dim"))
}
