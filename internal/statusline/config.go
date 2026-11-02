package statusline

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/edotau/claude-code/internal/ui/theme"
)

// Config is the render-time design: <ConfigDir>/state/statusline/config.json overlaid on the embedded default.
type Config struct {
	Style string `json:"style"` // gradient|powerline|capsule|minimal|dashboard
	theme.Look
	GradientEase   float64           `json:"gradientEase"` // ramp position exponent: <1 reaches the end colors sooner; 0 → 1
	UserColor      string            `json:"userColor"`    // "ansi" → literal PS1 bold-green; else palette user
	Order          []string          `json:"order"`
	BranchMax      int               `json:"branchMax"`
	PathMax        int               `json:"pathMax"` // cwd path display cap, in runes; 0 → no cap
	Width          int               `json:"width"`   // clip each line to N cells; 0 → $COLUMNS, else no clip
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

// Decoded on first use: this package links into every claude-code exec, only `statusline` reads it.
var embeddedConfig = sync.OnceValue(func() Config {
	var c Config
	_ = json.Unmarshal(theme.DefaultConfigJSON(), &c)
	return c
})

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

// loadConfig overlays config.json onto the embedded theme; maps the file omits keep the embedded ones.
func loadConfig() Config {
	cfg := embeddedConfig()
	data, err := os.ReadFile(filepath.Join(theme.StatuslineDir(), "config.json"))
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
