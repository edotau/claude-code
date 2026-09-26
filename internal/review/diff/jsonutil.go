package diff

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf16"
)

// pyFloat marshals like Python's json.dumps: shortest round-trip decimal, ".0" for integral values.
type pyFloat float64

func (f pyFloat) MarshalJSON() ([]byte, error) {
	s := strconv.FormatFloat(float64(f), 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return []byte(s), nil
}

func roundTo(x float64, places int) float64 {
	mult := math.Pow(10, float64(places))
	return math.Round(x*mult) / mult
}

func pyPercent(x float64) string {
	return fmt.Sprintf("%.0f%%", x*100)
}

// mustIndentJSON matches Python's json.dumps(v, indent=2): 2-space indent, ": " key separator,
// ensure_ascii escaping of every non-ASCII rune to \uXXXX, and no HTML-escaping of <, >, &.
func mustIndentJSON(v any) string {
	var enc bytes.Buffer
	e := json.NewEncoder(&enc)
	e.SetEscapeHTML(false)
	if err := e.Encode(v); err != nil {
		return fmt.Sprintf("{\"error\": %q}", err.Error())
	}
	compact := bytes.TrimRight(enc.Bytes(), "\n")
	ascii := escapeNonASCII(compact)
	spaced := insertColonSpace(ascii)
	var buf bytes.Buffer
	if err := json.Indent(&buf, spaced, "", "  "); err != nil {
		return string(spaced)
	}
	return buf.String()
}

// escapeNonASCII matches Python's json.dumps default ensure_ascii=True.
func escapeNonASCII(b []byte) []byte {
	var out bytes.Buffer
	for _, r := range string(b) {
		if r < 0x80 {
			out.WriteRune(r)
			continue
		}
		if r > 0xFFFF {
			r1, r2 := utf16.EncodeRune(r)
			fmt.Fprintf(&out, "\\u%04x\\u%04x", r1, r2)
		} else {
			fmt.Fprintf(&out, "\\u%04x", r)
		}
	}
	return out.Bytes()
}

// insertColonSpace adds a space after each key-separator colon outside of string literals,
// matching Python's default (', ', ': ') separators (Go's compact Marshal omits the space).
func insertColonSpace(b []byte) []byte {
	out := make([]byte, 0, len(b)+len(b)/8)
	inString := false
	escaped := false
	for _, c := range b {
		out = append(out, c)
		if inString {
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case ':':
			out = append(out, ' ')
		}
	}
	return out
}
