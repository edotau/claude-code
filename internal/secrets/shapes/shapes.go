// Package shapes is the one credential-shape heuristic shared by every printer and scanner.
package shapes

import (
	"regexp"
	"strings"
)

// bearerToken matches a bearer-style header value: the scheme plus a long opaque token.
var bearerToken = regexp.MustCompile(`(?i)bearer\s+\S{40,}`)

// Looks reports a credential-shaped value: known token prefixes or a bearer-style value; ${ENV}
// references, file paths, and obvious placeholders are never flagged.
func Looks(val string) bool {
	switch {
	case val == "":
		return false
	case strings.HasPrefix(val, "$"): // ${ENV} reference
		return false
	case strings.HasPrefix(val, "/") || strings.HasPrefix(val, "~/"): // file path
		return false
	case strings.Contains(val, "PLACEHOLDER") || strings.Contains(val, "YOUR_") || strings.Contains(val, "xxx"):
		return false
	case strings.HasPrefix(val, "ghp_") || strings.HasPrefix(val, "github_pat_") || strings.HasPrefix(val, "gho_"):
		return true
	case matchSlack(val):
		return true
	case strings.HasPrefix(val, "AKIA"): // AWS
		return true
	case strings.HasPrefix(val, "dapi"): // Databricks PAT
		return true
	case strings.HasPrefix(val, "sk-"): // OpenAI-style
		return true
	case bearerToken.MatchString(val):
		return true
	default:
		return false
	}
}

// matchSlack matches a Slack bot/user token (xoxb-/xoxp-).
func matchSlack(v string) bool {
	if !strings.HasPrefix(v, "xox") || len(v) < 5 || v[4] != '-' {
		return false
	}
	return v[3] == 'b' || v[3] == 'p'
}

// RedactMap returns a copy of m with every credential-shaped value replaced by "REDACTED".
func RedactMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		if Looks(v) {
			v = "REDACTED"
		}
		out[k] = v
	}
	return out
}

// Any reports whether any value in m is credential-shaped.
func Any(m map[string]string) bool {
	for _, v := range m {
		if Looks(v) {
			return true
		}
	}
	return false
}
