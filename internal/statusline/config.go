package statusline

import (
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/ansi"
	"github.com/edotau/claude-code/internal/paths"
)

//go:embed default_config.json
var embeddedConfigJSON []byte

//go:embed default_palettes.json
var embeddedPalettesJSON []byte

// Config is the render-time design: <ConfigDir>/statusline/config.json overlaid on the embedded default.
type Config struct {
	Style          string            `json:"style"`     // gradient|powerline|capsule|minimal|dashboard
	ColorMode      string            `json:"colorMode"` // auto|truecolor|ansi256|ansi16
	Gradient       string            `json:"gradient"`  // named ramp in palettes.gradients
	UserColor      string            `json:"userColor"` // "ansi" → literal PS1 bold-green; else palette user
	Order          []string          `json:"order"`
	BranchMax      int               `json:"branchMax"`
	Width          int               `json:"width"` // clip each line to N cells; 0 → $COLUMNS, else no clip
	Segments       map[string]bool   `json:"segments"`
	Bar            BarConfig         `json:"bar"`
	Separators     map[string]string `json:"separators"`
	PowerlineTheme string            `json:"powerlineTheme"` // dark|nord|tokyo-night|rose-pine|light
	Charset        string            `json:"charset"`        // nerd → Nerd Font glyphs; text → ASCII fallback
	VelocityMode   string            `json:"velocityMode"`   // code = uncommitted git diff; raw = Claude Code's ledger
	Dashboard      DashboardConfig   `json:"dashboard"`
}

// DashboardConfig controls the two-line dashboard layout.
type DashboardConfig struct {
	Columns []string `json:"columns"`
	Gutter  int      `json:"gutter"`
	Headers bool     `json:"headers"`
	Width   int      `json:"width"` // right-align context to N; 0 → Config.Width/$COLUMNS, else 100
}

// BarConfig controls the context bar's width, glyphs, and trough tone.
type BarConfig struct {
	Width         int     `json:"width"`
	FilledGlyph   string  `json:"filledGlyph"`
	EmptyGlyph    string  `json:"emptyGlyph"`
	Percent       string  `json:"percent"`    // "after" shows " N%" after the bar
	TerminalBg    string  `json:"terminalBg"` // trough tone base
	Theme         string  `json:"theme"`      // auto|dark|light
	EmptyContrast float64 `json:"emptyContrast"`
}

// Palettes holds named gradient ramps, powerline themes (theme → key → {bg,fg}) and semantic roles.
type Palettes struct {
	Gradients map[string][]string                     `json:"gradients"`
	Powerline map[string]map[string]map[string]string `json:"powerline"`
	Semantic  map[string]string                       `json:"semantic"`
}

// Decoded on first use: this package links into every claude-code exec, only `statusline` reads them.
var embeddedConfig = sync.OnceValue(func() Config {
	var c Config
	_ = json.Unmarshal(embeddedConfigJSON, &c)
	return c
})

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

func (c Config) seg(key string, def bool) bool {
	if v, ok := c.Segments[key]; ok {
		return v
	}
	return def
}

func (c Config) sep(key, def string) string {
	if v, ok := c.Separators[key]; ok {
		return v
	}
	return def
}

// ConfigDir holds the optional config.json / palettes.json overrides.
func ConfigDir() string { return filepath.Join(paths.ConfigDir(), "statusline") }

// loadConfig overlays config.json onto the embedded theme; maps the file omits keep the embedded ones.
func loadConfig() Config {
	cfg := embeddedConfig()
	data, err := os.ReadFile(filepath.Join(ConfigDir(), "config.json"))
	if err != nil {
		return cfg
	}
	cfg.Segments, cfg.Separators = nil, nil
	_ = json.Unmarshal(data, &cfg)
	if len(cfg.Segments) == 0 {
		cfg.Segments = embeddedConfig().Segments
	}
	if len(cfg.Separators) == 0 {
		cfg.Separators = embeddedConfig().Separators
	}
	return cfg
}

// loadPalettes returns palettes.json beside config.json, else the embedded palette.
func loadPalettes() Palettes {
	data, err := os.ReadFile(filepath.Join(ConfigDir(), "palettes.json"))
	if err != nil {
		return embeddedPalettes()
	}
	var pal Palettes
	if json.Unmarshal(data, &pal) != nil {
		return embeddedPalettes()
	}
	for role, h := range pal.Semantic {
		if !paintableHex(h) {
			delete(pal.Semantic, role) // sem() then serves the baked tone, not a silent black
		}
	}
	return pal
}

func paintableHex(h string) bool {
	n := len(strings.TrimPrefix(h, "#"))
	_, _, ok := ansi.ParseHex(h)
	return ok && (n == 3 || n == 6)
}

func (p Palettes) sem(role string) string {
	if h := p.Semantic[role]; h != "" {
		return h
	}
	return bakedSemantic[role]
}

// colorMode honors an explicit colorMode, else sniffs COLORTERM (truecolor when advertised, else 256).
func colorMode(cfg Config) string {
	if m := cfg.ColorMode; m != "" && m != "auto" {
		return m
	}
	ct := strings.ToLower(os.Getenv("COLORTERM"))
	if strings.Contains(ct, "truecolor") || strings.Contains(ct, "24bit") {
		return ansi.TrueColor
	}
	return ansi.ANSI256
}
