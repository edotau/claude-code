package settings

import (
	"bytes"
	"encoding/json"
)

// MarshalNoEscape is json.Marshal with HTML-escaping disabled, so a foreign hook's literal "<"/">"/"&"
// survives unchanged instead of becoming "<" etc; compact, same as json.Marshal otherwise.
func MarshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte{'\n'}), nil
}

// MarshalIndentNoEscape is json.MarshalIndent without HTML-escaping (json.Indent only reformats
// whitespace, it never re-escapes the bytes MarshalNoEscape already produced).
func MarshalIndentNoEscape(v any) ([]byte, error) {
	compact, err := MarshalNoEscape(v)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := json.Indent(&out, compact, "", "  "); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
