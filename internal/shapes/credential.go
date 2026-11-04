package shapes

import (
	"fmt"
	"strings"
)

// tskeyPrefixes maps each tskey- kind to its human name, for the "looks like an ... key" hint.
var tskeyPrefixes = []struct {
	prefix, kind string
}{
	{"tskey-client-", "an OAuth client secret"},
	{"tskey-api-", "an API key"},
	{"tskey-auth-", "an auth key"},
}

// kindFor reports which tskey- kind val looks like, if any.
func kindFor(val string) (prefix, kind string, ok bool) {
	for _, p := range tskeyPrefixes {
		if strings.HasPrefix(val, p.prefix) {
			return p.prefix, p.kind, true
		}
	}
	return "", "", false
}

// credentialPrefixes maps each prefixed Tailscale credential to the tskey- prefix and kind name it must have.
var credentialPrefixes = map[string]struct{ prefix, kind string }{
	"TAILSCALE_OAUTH_CLIENT_SECRET": {"tskey-client-", "OAuth client secret"},
	"TAILSCALE_API_KEY":             {"tskey-api-", "API key"},
	"TAILSCALE_AUTHKEY":             {"tskey-auth-", "auth key"},
}

// ValidCredential reports whether value has the documented shape for the named Tailscale
// credential; names it doesn't know are always valid. Never echoes value in its error.
func ValidCredential(name, value string) error {
	want, prefixed := credentialPrefixes[name]
	if !prefixed && name != "TAILSCALE_OAUTH_CLIENT_ID" {
		return nil
	}
	if strings.ContainsAny(value, " \t\n\r") {
		return fmt.Errorf("%s: must not contain whitespace", name)
	}
	if prefixed {
		return wantPrefix(name, value, want.prefix, want.kind)
	}
	return validClientID(name, value)
}

// validClientID checks a Tailscale OAuth client ID: k-prefixed, 10-40 alphanumeric chars.
func validClientID(name, value string) error {
	if len(value) >= 10 && len(value) <= 40 && strings.HasPrefix(value, "k") && isAlnum(value) {
		return nil
	}
	if prefix, kind, ok := kindFor(value); ok {
		return fmt.Errorf("%s: looks like %s (%s); want a client ID (starts with k, 10-40 alnum chars)", name, kind, prefix)
	}
	return fmt.Errorf("%s: want a client ID (starts with k, 10-40 alphanumeric chars)", name)
}

// isAlnum reports whether s is ASCII letters/digits only.
func isAlnum(s string) bool {
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// wantPrefix errors unless value has the given tskey- prefix, naming the kind it looks like instead.
func wantPrefix(name, value, prefix, kind string) error {
	if strings.HasPrefix(value, prefix) {
		return nil
	}
	if gotPrefix, gotKind, ok := kindFor(value); ok && gotPrefix != prefix {
		return fmt.Errorf("%s: looks like %s (%s); want %s", name, gotKind, gotPrefix, prefix)
	}
	return fmt.Errorf("%s: want %s prefix", name, prefix)
}
