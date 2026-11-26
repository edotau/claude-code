package redact

import (
	"regexp"
	"sync"
)

// HeaderMask replaces a sensitive header value.
const HeaderMask = "REDACTED"

var (
	queryParamSecret = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`(?i)([?&][\w-]*(?:token|key|passw(?:or)?d|secret|sig|auth)=)[^&\s]+`)
	})
	urlUserinfo = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?i)(\b[a-z][a-z0-9+.-]*://[^/\s:@]+:)[^/\s]+@`) })
)

// URL masks the password in URL userinfo and secret-named query values, keeping scheme, host and path.
func URL(s string) string {
	return queryParamSecret().ReplaceAllString(urlUserinfo().ReplaceAllString(s, "${1}"+HeaderMask+"@"), "${1}"+HeaderMask)
}
