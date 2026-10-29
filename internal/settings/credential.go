package settings

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/edotau/claude-code/internal/secrets/shapes"
)

// camelSecret matches settings names that hold or fetch a credential (apiKeyHelper, awsCredentialExport).
var camelSecret = regexp.MustCompile(`(?i)(apikey|credential|secret|password|token)`)

// authHeader matches a literal credential-bearing HTTP header at the start of any line or "\\n"-joined segment.
var authHeader = regexp.MustCompile(`(?i)(^|[\r\n]|\\n)\s*(proxy-authorization|authorization|x-api-key|api-key|cookie)\s*:`)

// CredentialKey reports a key whose value is a credential by name: it must never land in a plain file.
func CredentialKey(name string) bool {
	if !strings.Contains(name, "_") {
		return camelSecret.MatchString(name)
	}
	for seg := range strings.SplitSeq(strings.ToUpper(name), "_") {
		switch seg {
		case "TTL", "MS":
			return false
		}
	}
	for seg := range strings.SplitSeq(strings.ToUpper(name), "_") {
		switch seg {
		case "KEY", "TOKEN", "SECRET", "PASSWORD", "PASSWD", "CREDENTIALS":
			return true
		}
	}
	return false
}

// checkNotCredential refuses a credential-named key or a credential-shaped string value.
func checkNotCredential(key string, raw json.RawMessage) error {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	if CredentialKey(key) || credentialIn(v) {
		return fmt.Errorf("%s looks like a credential; store it with `claude-code vault set` or a provider auth.command", key)
	}
	return nil
}

// credentialIn walks nested values: a credential-named key or a credential-shaped string at any depth.
func credentialIn(v any) bool {
	switch t := v.(type) {
	case string:
		return authHeader.MatchString(t) || shapes.Looks(t) || slices.ContainsFunc(strings.FieldsFunc(t, splitToken), shapes.Looks)
	case []any:
		return slices.ContainsFunc(t, credentialIn)
	case map[string]any:
		for k, x := range t {
			if CredentialKey(k) || credentialIn(x) {
				return true
			}
		}
	}
	return false
}

// splitToken separates the words of a string value: whitespace, ':', '=', ',', ';' and quotes.
func splitToken(r rune) bool {
	return unicode.IsSpace(r) || strings.ContainsRune(`:=,;"'`, r)
}
