// Package pyjson emulates Python's json.dumps byte output (ensure_ascii, indent=2) for the review ports.
package pyjson

import (
	"bytes"
	"encoding/json"
	"fmt"
	"unicode/utf16"
)

// MarshalIndent matches Python's json.dumps(v, indent=2): ensure_ascii escaping and no HTML-escaping of <, >, &.
func MarshalIndent(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	compact := bytes.TrimRight(buf.Bytes(), "\n")
	escaped := EscapeNonASCII(compact)
	var out bytes.Buffer
	if err := json.Indent(&out, escaped, "", "  "); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// EscapeNonASCII matches Python's json.dumps default ensure_ascii=True (surrogate pairs above U+FFFF).
func EscapeNonASCII(b []byte) []byte {
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
