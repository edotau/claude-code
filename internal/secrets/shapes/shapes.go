// Package shapes is the one credential-shape heuristic shared by every printer and scanner.
package shapes

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// bearerToken matches a bearer-style header value: the scheme plus a long opaque token.
var bearerToken = regexp.MustCompile(`(?i)bearer\s+\S{40,}`)

// bearerAuth captures the scheme so Redact masks only the token after it.
var bearerAuth = regexp.MustCompile(`(?i)(bearer\s+)\S{40,}`)

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
	case HasGitHubPrefix(val):
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

// GitHubPrefixes are every GitHub token family: OAuth, PAT, user-to-server, server-to-server, refresh, fine-grained.
var GitHubPrefixes = []string{"gho_", "ghp_", "ghu_", "ghs_", "ghr_", "github_pat_"}

// HasGitHubPrefix reports a value starting with any GitHub token prefix.
func HasGitHubPrefix(v string) bool {
	return slices.ContainsFunc(GitHubPrefixes, func(p string) bool { return strings.HasPrefix(v, p) })
}

// GitHubPattern is a regex alternation matching any GitHub token with at least minLen body characters.
func GitHubPattern(minLen int) string {
	alts := make([]string, len(GitHubPrefixes))
	for i, p := range GitHubPrefixes {
		alts[i] = regexp.QuoteMeta(p) + "[A-Za-z0-9_]{" + strconv.Itoa(minLen) + ",}"
	}
	return "(?:" + strings.Join(alts, "|") + ")"
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

// Redact replaces each credential-shaped [A-Za-z0-9_-] run and bearer token in s with REDACTED, in place.
func Redact(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		j := i
		for j < len(s) && tokenByte(s[j]) {
			j++
		}
		if j == i {
			b.WriteByte(s[i])
			i++
			continue
		}
		if run := s[i:j]; Looks(run) {
			b.WriteString("REDACTED")
		} else {
			b.WriteString(run)
		}
		i = j
	}
	return bearerAuth.ReplaceAllString(b.String(), "${1}REDACTED")
}

// tokenByte reports a byte in the token alphabet every known prefix uses.
func tokenByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
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
