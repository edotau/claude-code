// Python json.dumps byte emulation (ensure_ascii, indent=2) for the review ports.

package source

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"unicode/utf16"

	"github.com/edotau/claude-code/internal/textutil"
)

// PyMarshalIndent matches Python's json.dumps(v, indent=2): ensure_ascii escaping and no HTML-escaping of <, >, &.
func PyMarshalIndent(v any) ([]byte, error) {
	compact, err := pyMarshalCompact(v)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := json.Indent(&out, compact, "", "  "); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// WritePyJSON writes v as PyMarshalIndent plus a newline; the caller words the error.
func WritePyJSON(w io.Writer, v any) error {
	b, err := PyMarshalIndent(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(b))
	return err
}

// pyMarshalCompact is PyMarshalIndent's single-line form: no HTML-escaping, ensure_ascii escaping.
func pyMarshalCompact(v any) ([]byte, error) {
	b, err := textutil.MarshalNoEscape(v)
	if err != nil {
		return nil, err
	}
	return pyEscapeNonASCII(b), nil
}

// pyEscapeNonASCII matches Python's json.dumps default ensure_ascii=True (surrogate pairs above U+FFFF).
func pyEscapeNonASCII(b []byte) []byte {
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
