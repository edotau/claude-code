package textutil

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
)

// MarshalNoEscape marshals compact JSON without HTML escaping or a trailing newline.
func MarshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte{'\n'}), nil
}

// MarshalIndentNoEscape marshals two-space-indented JSON without HTML escaping or a trailing newline.
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

// JWTClaims decodes the payload of a three-part JWT-shaped token without verifying its signature.
func JWTClaims(token string, into any) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	return err == nil && json.Unmarshal(payload, into) == nil
}
