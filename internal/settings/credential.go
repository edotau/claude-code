package settings

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/secrets/shapes"
)

// camelSecret matches settings names that hold or fetch a credential (apiKeyHelper, awsCredentialExport).
var camelSecret = regexp.MustCompile(`(?i)(apikey|credential|secret|password|token)`)

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
		return fmt.Errorf("%s looks like a credential; store it with `claude-code vault set`", key)
	}
	return nil
}

// credentialIn walks nested values: a credential-named key or a credential-shaped string at any depth.
func credentialIn(v any) bool {
	switch t := v.(type) {
	case string:
		return shapes.Looks(t)
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
