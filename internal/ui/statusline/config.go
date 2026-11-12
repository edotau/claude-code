package statusline

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"sync"

	"github.com/edotau/claude-code/internal/ui/theme"
)

// config is the render-time design: <ConfigDir>/state/statusline/config.json overlaid on the embedded default.
type config struct {
	Style string `json:"style"` // one of Styles
	theme.Look
	GradientEase   float64           `json:"gradientEase"` // ramp position exponent: <1 reaches the end colors sooner; 0 → 1
	UserColor      string            `json:"userColor"`    // "ansi" → literal PS1 bold-green; else palette user
	Order          []string          `json:"order"`
	ProfileMax     int               `json:"profileMax"` // profile-name cap in the provider label, in runes; 0 → no cap
	Width          int               `json:"width"`      // clip each line to N cells; 0 → $COLUMNS, else no clip
	Segments       map[string]bool   `json:"segments"`
	Bar            barConfig         `json:"bar"`
	Separators     map[string]string `json:"separators"`
	PowerlineTheme string            `json:"powerlineTheme"` // dark|nord|tokyo-night|rose-pine|light
	Charset        string            `json:"charset"`        // nerd → Nerd Font glyphs; text → ASCII fallback
	VelocityMode   string            `json:"velocityMode"`   // code = uncommitted git diff; raw = Claude Code's ledger
	Dashboard      dashboardConfig   `json:"dashboard"`
}

// dashboardConfig controls the two-line dashboard layout.
type dashboardConfig struct {
	Columns []string `json:"columns"`
	Gutter  int      `json:"gutter"`
	Headers bool     `json:"headers"`
	Width   int      `json:"width"` // right-align context to N; 0 → Config.Width/$COLUMNS, else 100
}

// barConfig controls the context bar's width, glyphs, and trough tone.
type barConfig struct {
	Width         int     `json:"width"`
	FilledGlyph   string  `json:"filledGlyph"`
	EmptyGlyph    string  `json:"emptyGlyph"`
	Percent       string  `json:"percent"`    // "before" puts N% ahead of the bar; anything else after it
	TerminalBg    string  `json:"terminalBg"` // trough tone base
	Theme         string  `json:"theme"`      // auto|dark|light
	EmptyContrast float64 `json:"emptyContrast"`
}

// defaultConfig decodes a fresh embedded theme: json reuses a slice's backing array, so overlays must not share one.
func defaultConfig() config {
	var c config
	_ = json.Unmarshal(theme.DefaultConfigJSON(), &c)
	return c
}

// Decoded on first use: this package links into every claude-code exec, only `statusline` reads it.
var embeddedConfig = sync.OnceValue(defaultConfig)

// seg reports whether segment key is on: enabled unless config.json sets it false.
func (c config) seg(key string) bool {
	if v, ok := c.Segments[key]; ok {
		return v
	}
	return true
}

func (c config) sep(key, def string) string {
	if v, ok := c.Separators[key]; ok {
		return v
	}
	return def
}

// loadConfig overlays config.json onto the embedded theme; a file that fails to decode is ignored whole.
func loadConfig() config {
	cfg := embeddedConfig()
	data, err := os.ReadFile(filepath.Join(theme.StatuslineDir(), "config.json"))
	if err != nil {
		return cfg
	}
	user := defaultConfig()
	user.Segments, user.Separators = nil, nil
	if err := json.Unmarshal(data, &user); err != nil {
		return cfg
	}
	merged := make(map[string]bool, len(cfg.Segments)+len(user.Segments))
	maps.Copy(merged, cfg.Segments)
	maps.Copy(merged, user.Segments) // a key the user did not name keeps the embedded value
	user.Segments = merged
	seps := maps.Clone(cfg.Separators)
	maps.Copy(seps, user.Separators) // a separator the user did not name keeps the embedded one
	user.Separators = seps
	return user
}

// gradientStyle is true for the default style: any Style renderRaw does not route elsewhere.
func (c config) gradientStyle() bool {
	switch c.Style {
	case "twoline", "dashboard", "powerline", "capsule", "minimal":
		return false
	}
	return true
}
