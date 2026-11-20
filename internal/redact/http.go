package redact

import (
	"net/http"
	"regexp"
	"slices"
	"strings"
)

// HeaderMask replaces a sensitive header value.
const HeaderMask = "REDACTED"

var (
	sensitiveExact  = []string{"authorization", "proxy-authorization", "cookie", "set-cookie", "x-api-key", "api-key"}
	sensitiveSuffix = []string{"-token", "-key", "-secret", "-signature", "-password"}
	sensitiveSubstr = []string{"token", "secret", "password", "credential"}

	queryParamSecret = regexp.MustCompile(`(?i)([?&][\w-]*(?:token|key|passw(?:or)?d|secret|sig|auth)=)[^&\s]+`)
	urlUserinfo      = regexp.MustCompile(`(?i)(\b[a-z][a-z0-9+.-]*://[^/\s:@]+:)[^/\s]+@`)
)

// SensitiveHeader reports a header name that carries a credential (exact, suffix or substring match).
func SensitiveHeader(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	return n != "" && (slices.Contains(sensitiveExact, n) ||
		hasAny(n, sensitiveSuffix, strings.HasSuffix) || hasAny(n, sensitiveSubstr, strings.Contains))
}

// Headers returns a copy of h with sensitive values masked.
func Headers(h http.Header) http.Header {
	out := make(http.Header, len(h))
	for k, vs := range h {
		if !SensitiveHeader(k) {
			out[k] = append([]string(nil), vs...)
			continue
		}
		out[k] = make([]string, len(vs))
		for i := range vs {
			out[k][i] = HeaderMask
		}
	}
	return out
}

// URL masks the password in URL userinfo and secret-named query values, keeping scheme, host and path.
func URL(s string) string {
	return queryParamSecret.ReplaceAllString(urlUserinfo.ReplaceAllString(s, "${1}"+HeaderMask+"@"), "${1}"+HeaderMask)
}

func hasAny(s string, list []string, match func(string, string) bool) bool {
	for _, v := range list {
		if match(s, v) {
			return true
		}
	}
	return false
}
