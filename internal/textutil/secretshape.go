package textutil

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// bearerToken matches a bearer-style header value: the scheme plus a long opaque token.
var bearerToken = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?i)bearer\s+\S{40,}`) })

// bearerAuth captures the scheme so RedactSecrets masks only the token after it.
var bearerAuth = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?i)(bearer\s+)\S{40,}`) })

// jwtToken matches a dotted JWT (header.payload, optional signature), which the run pass splits at its dots.
var jwtToken = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{16,}\.[A-Za-z0-9_-]{16,}(?:\.[A-Za-z0-9_-]*)?`)
})

// pemPrivateKey matches a private-key block through its END line, or to end of input when truncated.
var pemPrivateKey = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?(?:-----END [A-Z ]*PRIVATE KEY-----|\z)`)
})

// CredentialRE is the credential shape the memory commit gate and config history refuse; \b keeps sk- off kebab prose.
var CredentialRE = regexp.MustCompile(strings.Join([]string{
	`\bsk-[A-Za-z0-9_-]{20,}`,                     // OpenAI/Anthropic, incl. sk-proj-/sk-ant- segments
	`\bdapi[0-9a-f]{32}`,                          // Databricks PAT
	`\beyJ[A-Za-z0-9_-]{16,}\.[A-Za-z0-9_-]{16,}`, // JWT (header.payload)
	`\bnvapi-[A-Za-z0-9_-]{24,}`,                  // NVIDIA
	`\bgh[pousr]_[A-Za-z0-9]{28,}`,                // GitHub
	`\bgithub_pat_[A-Za-z0-9_]{22,}`,              // GitHub fine-grained PAT
	`\bAIza[0-9A-Za-z_-]{35}`,                     // Google API key
	`\btvly-[A-Za-z0-9_-]{16,}`,                   // Tavily
	`\bgsk_[A-Za-z0-9]{24,}`,                      // Groq
	`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`,               // AWS access key id (long-term, STS)
	`\bxox[abprs]-[A-Za-z0-9-]{10,}`,              // Slack
	`-----BEGIN [A-Z ]*PRIVATE KEY`,               // private keys only, never certificates or public keys
}, "|"))

// LooksSecret reports a credential-shaped value: known token prefixes or a bearer-style value; ${ENV}
// references, file paths, and obvious placeholders are never flagged.
func LooksSecret(val string) bool {
	return looksSecretToken(val) || val != "" && !notCredentialShaped(val) && bearerToken().MatchString(val)
}

// looksSecretToken is LooksSecret's prefix checks without the bearer regex, which cannot match a whitespace-free run.
func looksSecretToken(val string) bool {
	if val == "" || notCredentialShaped(val) {
		return false
	}
	return HasGitHubPrefix(val) || IsSlackToken(val) || startsCredential(val)
}

// startsCredential reports a value that begins with a full CredentialRE shape (Databricks, Tavily, Groq, JWT, ...).
func startsCredential(val string) bool {
	loc := CredentialRE.FindStringIndex(val)
	return loc != nil && loc[0] == 0
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

// gitHubPrefixes are every GitHub token family: OAuth, PAT, user-to-server, server-to-server, refresh, fine-grained.
var gitHubPrefixes = []string{"gho_", "ghp_", "ghu_", "ghs_", "ghr_", "github_pat_"}

// HasGitHubPrefix reports a value starting with any GitHub token prefix.
func HasGitHubPrefix(v string) bool {
	return hasAnyPrefix(v, gitHubPrefixes...)
}

// GitHubPattern is a regex alternation matching any GitHub token with at least minLen body characters.
func GitHubPattern(minLen int) string {
	alts := make([]string, len(gitHubPrefixes))
	for i, p := range gitHubPrefixes {
		alts[i] = regexp.QuoteMeta(p) + "[A-Za-z0-9_]{" + strconv.Itoa(minLen) + ",}"
	}
	return "(?:" + strings.Join(alts, "|") + ")"
}

// IsSlackToken matches a Slack bot/user/app/legacy token (xox[baprs]-...).
func IsSlackToken(v string) bool {
	if len(v) < 5 || !strings.HasPrefix(v, "xox") || v[4] != '-' {
		return false
	}
	return strings.ContainsRune("baprs", rune(v[3]))
}

// RedactSecretMap returns a copy of m with every credential-shaped value replaced by "REDACTED".
func RedactSecretMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		if LooksSecret(v) {
			v = "REDACTED"
		}
		out[k] = v
	}
	return out
}

// RedactSecrets replaces each credential-shaped run, bearer token, JWT and PEM key block in s with REDACTED.
func RedactSecrets(s string) string {
	s = pemPrivateKey().ReplaceAllString(s, "REDACTED")
	s = jwtToken().ReplaceAllString(s, "REDACTED")
	s = bearerAuth().ReplaceAllString(s, "${1}REDACTED") // before the run pass, which would shorten the token
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
		if run := s[i:j]; looksSecretToken(run) {
			b.WriteString("REDACTED")
		} else {
			b.WriteString(run)
		}
		i = j
	}
	return b.String()
}

// tokenByte reports a byte in the token alphabet every known prefix uses.
func tokenByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

// AnySecret reports whether any value in m is credential-shaped.
func AnySecret(m map[string]string) bool {
	for _, v := range m {
		if LooksSecret(v) {
			return true
		}
	}
	return false
}

// PreviewRedacted collapses prompt's whitespace, redacts credential-shaped words, and truncates to n runes.
func PreviewRedacted(prompt string, n int) string {
	return Truncate(RedactSecrets(strings.Join(strings.Fields(prompt), " ")), n)
}
