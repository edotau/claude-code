package textutil

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// DisplayWidth counts terminal cells; controls count zero, so callers must expand tabs first.
func DisplayWidth(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] >= 0x7f {
			start := max(i-1, 0)
			return start + cellsWidth(s[start:])
		}
	}
	return len(s)
}

func cellsWidth(s string) int {
	width := 0
	for i := 0; i < len(s); {
		end, w := cellToken(s, i)
		width += w
		i = end
	}
	return width
}

// TruncateCells includes marker in the budget; an oversized marker is itself clipped without a marker.
func TruncateCells(s string, n int, marker string) string {
	if n <= 0 {
		return ""
	}
	width := DisplayWidth(s)
	if width <= n {
		return s
	}
	markerWidth := DisplayWidth(marker)
	if markerWidth > n {
		return TruncateCells(marker, n, "")
	}
	// For valid UTF-8, equal byte and cell counts imply ASCII, so clipping can use a byte slice.
	if width == len(s) {
		return s[:n-markerWidth] + marker
	}
	end := cellPrefix(s, n-markerWidth)
	reset := ""
	if strings.Contains(s[:end], "\x1b") {
		reset = "\x1b[0m"
	}
	return s[:end] + marker + reset
}

func cellPrefix(s string, n int) int {
	i, width := 0, 0
	for i < len(s) {
		end, w := cellToken(s, i)
		if width+w > n {
			break
		}
		if s[i] == 0x1b && !completeEscape(s[i:end]) {
			break
		}
		width += w
		i = end
	}
	return i
}

// PadCells pads on the right, or on the left when left is true for numeric columns.
func PadCells(s string, n int, left bool) string {
	gap := n - DisplayWidth(s)
	if gap <= 0 {
		return s
	}
	pad := strings.Repeat(" ", gap)
	if left {
		return pad + s
	}
	return s + pad
}

func cellToken(s string, i int) (int, int) {
	if s[i] == 0x1b {
		return cellEscapeEnd(s, i), 0
	}
	r, size := utf8.DecodeRuneInString(s[i:])
	end, width := i+size, runeCells(r)
	regional := regionalIndicator(r)
	joined := false
	for end < len(s) {
		next, size := utf8.DecodeRuneInString(s[end:])
		switch {
		case next == 0x200d:
			joined = true
		case next == 0xfe0f:
			if width > 0 {
				width = max(width, 2)
			}
		case zeroCellRune(next), emojiModifier(next):
		case joined:
			width = max(width, runeCells(next))
			joined = false
		case regional && regionalIndicator(next):
			width, regional = 2, false
		default:
			return end, width
		}
		end += size
	}
	return end, width
}

func cellEscapeEnd(s string, i int) int {
	if i+1 >= len(s) {
		return len(s)
	}
	switch s[i+1] {
	case '[':
		for j := i + 2; j < len(s); j++ {
			if s[j] >= 0x40 && s[j] <= 0x7e {
				return j + 1
			}
		}
	case ']':
		return cellOSCEnd(s, i+2)
	default:
		return i + 2
	}
	return len(s)
}

func cellOSCEnd(s string, start int) int {
	for j := start; j < len(s); j++ {
		if s[j] == 7 {
			return j + 1
		}
		if s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\' {
			return j + 2
		}
	}
	return len(s)
}

func regionalIndicator(r rune) bool { return r >= 0x1f1e6 && r <= 0x1f1ff }
func emojiModifier(r rune) bool     { return r >= 0x1f3fb && r <= 0x1f3ff }

func completeEscape(s string) bool {
	if len(s) < 2 {
		return false
	}
	switch s[1] {
	case '[':
		last := s[len(s)-1]
		return len(s) > 2 && last >= 0x40 && last <= 0x7e
	case ']':
		return strings.HasSuffix(s, "\a") || strings.HasSuffix(s, "\x1b\\")
	}
	return true
}

func zeroCellRune(r rune) bool {
	return unicode.IsMark(r) || unicode.Is(unicode.Cf, r)
}

// ZWJ clusters use the common two-cell emoji model; ambiguous widths stay one, without locale tailoring.
func runeCells(r rune) int {
	if r < 0x20 || r >= 0x7f && r < 0xa0 {
		return 0
	}
	if r < 0x7f {
		return 1
	}
	if zeroCellRune(r) {
		return 0
	}
	lo, hi := 0, len(cellWideRanges)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if cellWideRanges[mid][1] < r {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo < len(cellWideRanges) && cellWideRanges[lo][0] <= r {
		return 2
	}
	return 1
}

// Compact wide ranges adapted from the existing statusline table; unlisted scripts and emoji default to one.
var cellWideRanges = [][2]rune{
	{0x1100, 0x115F}, {0x231A, 0x231B}, {0x23E9, 0x23EC}, {0x23F0, 0x23F0}, {0x23F3, 0x23F3},
	{0x25FD, 0x25FE}, {0x2614, 0x2615}, {0x2648, 0x2653}, {0x267F, 0x267F}, {0x2693, 0x2693},
	{0x26A1, 0x26A1}, {0x26AA, 0x26AB}, {0x26BD, 0x26BE}, {0x26C4, 0x26C5}, {0x26CE, 0x26CE},
	{0x26D4, 0x26D4}, {0x26EA, 0x26EA}, {0x26F2, 0x26F3}, {0x26F5, 0x26F5}, {0x26FA, 0x26FA},
	{0x26FD, 0x26FD}, {0x2705, 0x2705}, {0x270A, 0x270B}, {0x2728, 0x2728}, {0x274C, 0x274C},
	{0x274E, 0x274E}, {0x2753, 0x2755}, {0x2757, 0x2757}, {0x2795, 0x2797}, {0x27B0, 0x27B0},
	{0x27BF, 0x27BF}, {0x2B1B, 0x2B1C}, {0x2B50, 0x2B50}, {0x2B55, 0x2B55}, {0x2E80, 0x303E},
	{0x3040, 0x3247}, {0x3250, 0x4DBF}, {0x4E00, 0xA4CF},
	{0xAC00, 0xD7A3}, {0xF900, 0xFAFF}, {0xFE30, 0xFE4F}, {0xFF01, 0xFF60}, {0xFFE0, 0xFFE6},
	{0x1B000, 0x1B2FF}, {0x1F004, 0x1F004}, {0x1F0CF, 0x1F0CF}, {0x1F18E, 0x1F18E}, {0x1F191, 0x1F19A},
	{0x1F1E6, 0x1F1FF}, {0x1F300, 0x1F64F}, {0x1F680, 0x1F6FF}, {0x1F7E0, 0x1F7EB}, {0x1F900, 0x1F9FF},
	{0x1FA70, 0x1FAFF}, {0x20000, 0x3FFFD},
}
