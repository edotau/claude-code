package diff

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/review/source"
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
	s, err := indentJSON(v)
	if err != nil {
		return fmt.Sprintf("{\"error\": %q}", err.Error())
	}
	return s
}

// indentJSON is mustIndentJSON that returns the encode error instead of an error-shaped document.
func indentJSON(v any) (string, error) {
	var enc bytes.Buffer
	e := json.NewEncoder(&enc)
	e.SetEscapeHTML(false)
	if err := e.Encode(v); err != nil {
		return "", err
	}
	compact := bytes.TrimRight(enc.Bytes(), "\n")
	ascii := source.PyEscapeNonASCII(compact)
	spaced := insertColonSpace(ascii)
	var buf bytes.Buffer
	if err := json.Indent(&buf, spaced, "", "  "); err != nil {
		return string(spaced), nil
	}
	return buf.String(), nil
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
			inString, escaped = stepString(c, escaped)
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

// stepString advances the in-string scan by one byte and returns the new (inString, escaped) state.
func stepString(c byte, escaped bool) (inString, esc bool) {
	switch {
	case escaped:
		return true, false
	case c == '\\':
		return true, true
	case c == '"':
		return false, false
	}
	return true, false
}
