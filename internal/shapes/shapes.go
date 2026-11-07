// Package shapes is the one credential-shape heuristic shared by every printer and scanner.
package shapes

import (
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/textutil"
)

// bearerToken matches a bearer-style header value: the scheme plus a long opaque token.
var bearerToken = regexp.MustCompile(`(?i)bearer\s+\S{40,}`)

// bearerAuth captures the scheme so Redact masks only the token after it.
var bearerAuth = regexp.MustCompile(`(?i)(bearer\s+)\S{40,}`)

// Looks reports a credential-shaped value: known token prefixes or a bearer-style value; ${ENV}
// references, file paths, and obvious placeholders are never flagged.
func Looks(val string) bool {
	if val == "" || notCredentialShaped(val) {
		return false
	}
	return HasGitHubPrefix(val) || matchSlack(val) || hasAnyPrefix(val, "AKIA", "dapi", "sk-") || bearerToken.MatchString(val)
}

// placeholderWindow is how far into a value a placeholder marker counts; later hits are random token bytes.
const placeholderWindow = 16

// notCredentialShaped reports an env reference, file path or obvious placeholder.
func notCredentialShaped(val string) bool {
	head := val[:min(len(val), placeholderWindow)]
	return strings.HasPrefix(val, "$") || // ${ENV} reference
		strings.HasPrefix(val, "/") || strings.HasPrefix(val, "~/") || // file path
		strings.Contains(head, "PLACEHOLDER") || strings.Contains(head, "YOUR_") || strings.Contains(head, "xxx")
}

// hasAnyPrefix reports whether v starts with any of prefixes (AWS, Databricks PAT, OpenAI-style).
func hasAnyPrefix(v string, prefixes ...string) bool {
	return slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(v, p) })
}

// GitHubPrefixes are every GitHub token family: OAuth, PAT, user-to-server, server-to-server, refresh, fine-grained.
var GitHubPrefixes = []string{"gho_", "ghp_", "ghu_", "ghs_", "ghr_", "github_pat_"}

// HasGitHubPrefix reports a value starting with any GitHub token prefix.
func HasGitHubPrefix(v string) bool {
	return hasAnyPrefix(v, GitHubPrefixes...)
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

// Preview collapses prompt's whitespace, redacts credential-shaped words, and truncates to n runes.
func Preview(prompt string, n int) string {
	return textutil.Truncate(Redact(strings.Join(strings.Fields(prompt), " ")), n)
}
