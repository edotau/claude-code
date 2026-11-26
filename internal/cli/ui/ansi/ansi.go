// Package ansi renders SGR escapes in truecolor/256/16-color modes; a leaf that imports only internal/proc.
package ansi

import (
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
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
)

// ParseHex reads #RGB, #RGBA, #RRGGBB or #RRGGBBAA (# optional); alpha is 255 when absent, ok=false otherwise.
func ParseHex(h string) (rgb RGB, alpha int, ok bool) {
	h = strings.TrimPrefix(h, "#")
	w := 1 // hex digits per channel
	switch len(h) {
	case 3, 4:
	case 6, 8:
		w = 2
	default:
		return RGB{}, 0, false
	}
	c := [4]int{3: 255}
	for i := range len(h) / w {
		c[i] = 0
		for j := i * w; j < i*w+w; j++ {
			v, valid := hexNibble(h[j])
			if !valid {
				return RGB{}, 0, false
			}
			c[i] = c[i]*16 + v
		}
		if w == 1 {
			c[i] *= 17
		}
	}
	return RGB{c[0], c[1], c[2]}, c[3], true
}

func hexNibble(c byte) (int, bool) {
	switch {
	case '0' <= c && c <= '9':
		return int(c - '0'), true
	case 'a' <= c && c <= 'f':
		return int(c-'a') + 10, true
	case 'A' <= c && c <= 'F':
		return int(c-'A') + 10, true
	}
	return 0, false
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

// toANSI256 maps an RGB to the nearest xterm-256 index (grayscale ramp + 6×6×6 cube).
func toANSI256(rgb RGB) int {
	r, g, b := rgb[0], rgb[1], rgb[2]
	if abs(r-g) < 10 && abs(g-b) < 10 {
		if r < 8 {
			return 16
		}
		if r > 248 {
			return 231
		}
		return 232 + min(int(math.Round(float64(r-8)/10)), 23) // xterm grays are 8+10*i, i in 0..23
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

// toANSI16 returns the index of the nearest 16-color entry by squared RGB distance.
func toANSI16(rgb RGB) int {
	best, bi := math.MaxInt, 7
	for i, c := range ansi16 {
		d := (rgb[0]-c[0])*(rgb[0]-c[0]) + (rgb[1]-c[1])*(rgb[1]-c[1]) + (rgb[2]-c[2])*(rgb[2]-c[2])
		if d < best {
			best, bi = d, i
		}
	}
	return bi
}

// appendByteDec appends n (0..255) in base 10 without strconv's general loop.
func appendByteDec(dst []byte, n int) []byte {
	if n < 0 || n > 255 {
		return strconv.AppendInt(dst, int64(n), 10)
	}
	if n >= 100 {
		dst = append(dst, byte('0'+n/100))
		n %= 100
		return append(dst, byte('0'+n/10), byte('0'+n%10))
	}
	if n >= 10 {
		return append(dst, byte('0'+n/10), byte('0'+n%10))
	}
	return append(dst, byte('0'+n))
}

// AppendFgSeq appends the foreground SGR escape for rgb in mode to dst.
func AppendFgSeq(dst []byte, mode string, rgb RGB, bold bool) []byte {
	return appendSeq(dst, '3', mode, rgb, bold)
}

// appendSeq appends the SGR escape for rgb on layer '3' (foreground) or '4' (background); 16-color bright fg adds bold.
func appendSeq(dst []byte, layer byte, mode string, rgb RGB, bold bool) []byte {
	dst = append(dst, "\x1b["...)
	if bold {
		dst = append(dst, "1;"...)
	}
	switch mode {
	case TrueColor:
		dst = append(dst, layer, '8', ';', '2', ';')
		dst = appendByteDec(dst, rgb[0])
		dst = append(dst, ';')
		dst = appendByteDec(dst, rgb[1])
		dst = append(dst, ';')
		dst = appendByteDec(dst, rgb[2])
	case ANSI256:
		dst = append(dst, layer, '8', ';', '5', ';')
		dst = appendByteDec(dst, toANSI256(rgb))
	default:
		c := toANSI16(rgb)
		if c >= 8 {
			c -= 8
			if layer == '3' && !bold {
				dst = append(dst, "1;"...)
			}
		}
		dst = append(dst, layer, byte('0'+c))
	}
	return append(dst, 'm')
}

// FgSeq returns the foreground SGR escape for rgb in the given color mode.
func FgSeq(mode string, rgb RGB, bold bool) string {
	var buf [32]byte
	return string(AppendFgSeq(buf[:0], mode, rgb, bold))
}

// BgSeq returns the background SGR escape for rgb in the given color mode.
func BgSeq(mode string, rgb RGB) string {
	var buf [32]byte
	return string(appendSeq(buf[:0], '4', mode, rgb, false))
}

// Paint wraps s in the foreground color for rgb (optionally bold) and a reset.
func Paint(mode string, rgb RGB, s string, bold bool) string {
	var buf [64]byte
	b := AppendFgSeq(buf[:0], mode, rgb, bold)
	b = append(b, s...)
	b = append(b, Reset...)
	return string(b)
}

// Sweep paints s rune by rune across the gradient, bold, like the status line's path segment.
func Sweep(mode string, stops []RGB, s string) string {
	if s == "" {
		return Reset
	}
	n := utf8.RuneCountInString(s)
	denom := float64(max(n-1, 1))
	buf := make([]byte, 0, n*22+len(Reset))
	i := 0
	for _, r := range s {
		buf = AppendFgSeq(buf, mode, GradAt(stops, float64(i)/denom), true)
		buf = utf8.AppendRune(buf, r)
		i++
	}
	buf = append(buf, Reset...)
	return string(buf)
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// smooth is the smoothstep easing x*x*(3-2x), clamped to [0,1].
func smooth(x float64) float64 {
	x = math.Max(0, math.Min(1, x))
	return x * x * (3 - 2*x)
}

// TowardWhite lightens rgb by fraction w toward white.
func TowardWhite(rgb RGB, w float64) RGB { return Mix(rgb, RGB{255, 255, 255}, w) }

// Mix linearly interpolates a→b by fraction w.
func Mix(a, b RGB, w float64) RGB {
	return RGB{
		int(math.Round(float64(a[0]) + (float64(b[0])-float64(a[0]))*w)),
		int(math.Round(float64(a[1]) + (float64(b[1])-float64(a[1]))*w)),
		int(math.Round(float64(a[2]) + (float64(b[2])-float64(a[2]))*w)),
	}
}

// GradAt samples the gradient at frac in [0,1], smoothstep-eased; 0 stops is gray, 1 stop is flat.
func GradAt(stops []RGB, frac float64) RGB {
	n := len(stops)
	if n == 0 {
		return RGB{150, 150, 160}
	}
	if n == 1 {
		return stops[0]
	}
	// Uniform stop grid: arithmetic span index, no per-rune allocation.
	t := math.Max(0, math.Min(100, frac*100))
	step := 100 / float64(n-1)
	k := min(int(t/step), n-2)
	lt := smooth((t - float64(k)*step) / step)
	return Mix(stops[k], stops[k+1], lt)
}
