// Package ansi renders SGR escapes in truecolor/256/16-color modes; a stdlib-only leaf with no internal imports.
package ansi

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// RGB is a 24-bit color.
type RGB [3]int

// Reset clears all SGR attributes; Bold sets the bold attribute.
const (
	Reset = "\x1b[0m"
	Bold  = "\x1b[1m"
)

// Color modes accepted by FgSeq/BgSeq; anything else downsamples to 16 colors.
const (
	TrueColor = "truecolor"
	ANSI256   = "ansi256"
	ANSI16    = "ansi16"
)

// ParseHex reads #RGB, #RGBA, #RRGGBB or #RRGGBBAA (# optional); alpha is 255 when absent, ok=false otherwise.
func ParseHex(h string) (rgb RGB, alpha int, ok bool) {
	h = strings.TrimPrefix(h, "#")
	if len(h) == 3 || len(h) == 4 { // shorthand: each nibble doubles
		var wide strings.Builder
		for i := 0; i < len(h); i++ {
			wide.WriteByte(h[i])
			wide.WriteByte(h[i])
		}
		h = wide.String()
	}
	if len(h) != 6 && len(h) != 8 {
		return RGB{}, 0, false
	}
	var c [4]int
	c[3] = 255
	for i := 0; i < len(h)/2; i++ {
		n, err := strconv.ParseUint(h[2*i:2*i+2], 16, 8)
		if err != nil {
			return RGB{}, 0, false
		}
		c[i] = int(n)
	}
	return RGB{c[0], c[1], c[2]}, c[3], true
}

// Hex parses "#rgb" or "#rrggbb"; anything else reads as black (validate overrides with ParseHex at load).
func Hex(h string) RGB {
	if n := len(strings.TrimPrefix(h, "#")); n != 3 && n != 6 {
		return RGB{}
	}
	rgb, _, ok := ParseHex(h)
	if !ok {
		return RGB{}
	}
	return rgb
}

// ToANSI256 maps an RGB to the nearest xterm-256 index (grayscale ramp + 6×6×6 cube).
func ToANSI256(rgb RGB) int {
	r, g, b := rgb[0], rgb[1], rgb[2]
	if abs(r-g) < 10 && abs(g-b) < 10 {
		if r < 8 {
			return 16
		}
		if r > 248 {
			return 231
		}
		return int(math.Round(float64(r-8)/247*24)) + 232
	}
	q := func(v int) int {
		switch {
		case v < 48:
			return 0
		case v < 115:
			return 1
		default:
			return (v - 35) / 40
		}
	}
	return 16 + 36*q(r) + 6*q(g) + q(b)
}

// ansi16 is the standard 16-color palette used for nearest-match downsampling.
var ansi16 = []RGB{
	{0, 0, 0}, {205, 49, 49}, {13, 188, 121}, {229, 229, 16},
	{36, 114, 200}, {188, 63, 188}, {17, 168, 205}, {229, 229, 229},
	{102, 102, 102}, {241, 76, 76}, {35, 209, 139}, {245, 245, 67},
	{59, 142, 234}, {214, 112, 214}, {41, 184, 219}, {255, 255, 255},
}

// ToANSI16 returns the index of the nearest 16-color entry by squared RGB distance.
func ToANSI16(rgb RGB) int {
	best, bi := math.MaxInt, 7
	for i, c := range ansi16 {
		d := (rgb[0]-c[0])*(rgb[0]-c[0]) + (rgb[1]-c[1])*(rgb[1]-c[1]) + (rgb[2]-c[2])*(rgb[2]-c[2])
		if d < best {
			best, bi = d, i
		}
	}
	return bi
}

// FgSeq returns the foreground SGR escape for rgb in the given color mode.
func FgSeq(mode string, rgb RGB, bold bool) string {
	b := ""
	if bold {
		b = "1;"
	}
	switch mode {
	case TrueColor:
		return fmt.Sprintf("\x1b[%s38;2;%d;%d;%dm", b, rgb[0], rgb[1], rgb[2])
	case ANSI256:
		return fmt.Sprintf("\x1b[%s38;5;%dm", b, ToANSI256(rgb))
	default:
		c := ToANSI16(rgb)
		base := 30 + c
		if c >= 8 {
			base = 30 + c - 8
		}
		bright := ""
		if c >= 8 && !bold {
			bright = "1;"
		}
		return fmt.Sprintf("\x1b[%s%s%dm", b, bright, base)
	}
}

// BgSeq returns the background SGR escape for rgb in the given color mode.
func BgSeq(mode string, rgb RGB) string {
	switch mode {
	case TrueColor:
		return fmt.Sprintf("\x1b[48;2;%d;%d;%dm", rgb[0], rgb[1], rgb[2])
	case ANSI256:
		return fmt.Sprintf("\x1b[48;5;%dm", ToANSI256(rgb))
	default:
		c := ToANSI16(rgb)
		base := 40 + c
		if c >= 8 {
			base = 40 + c - 8
		}
		return fmt.Sprintf("\x1b[%dm", base)
	}
}

// Paint wraps s in the foreground color for rgb (optionally bold) and a reset.
func Paint(mode string, rgb RGB, s string, bold bool) string {
	return FgSeq(mode, rgb, bold) + s + Reset
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
