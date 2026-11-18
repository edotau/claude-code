package textutil

import (
	"regexp"
	"regexp/syntax"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
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
var CredentialRE = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(strings.Join([]string{
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
})

// LooksSecret reports a credential-shaped value
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
	loc := CredentialRE().FindStringIndex(val)
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

// HasAnyPrefix reports whether v starts with any of prefixes.
func HasAnyPrefix(v string, prefixes ...string) bool {
	return slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(v, p) })
}

// gitHubPrefixes are every GitHub token family: OAuth, PAT, user-to-server, server-to-server, refresh, fine-grained.
var gitHubPrefixes = []string{"gho_", "ghp_", "ghu_", "ghs_", "ghr_", "github_pat_"}

// HasGitHubPrefix reports a value starting with any GitHub token prefix.
func HasGitHubPrefix(v string) bool {
	return HasAnyPrefix(v, gitHubPrefixes...)
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
	s := strings.Join(strings.Fields(prompt), " ")
	if n <= 0 || len(s) <= n {
		return Truncate(RedactSecrets(s), n)
	}
	k := 0
	for k < len(s) {
		k = runeOffset(s, k, n+1)
		for k < len(s) && tokenByte(s[k]) {
			k++
		}
		if k < len(s) && farMarker(s[:min(len(s), k+len("-----BEGIN")-1)]) {
			k = len(s)
		}
		out := RedactSecrets(s[:k])
		if k == len(s) || utf8.RuneCountInString(out) > n {
			return Truncate(out, n)
		}
	}
	return Truncate(RedactSecrets(s), n)
}

// runeOffset is the byte offset in s after count more runes from from, or len(s).
func runeOffset(s string, from, count int) int {
	for i := range s[from:] {
		if count--; count < 0 {
			return from + i
		}
	}
	return len(s)
}

// farMarker reports a start of a pem, jwt or bearer match, which can reach far past a cut (redaction can shorten text).
func farMarker(s string) bool {
	return strings.Contains(s, "-----BEGIN") || strings.Contains(s, "eyJ") ||
		strings.Contains(strings.ToLower(s), "bearer")
}

// SecretSpans preserves byte offsets so callers can overwrite secrets without moving stored records.
func SecretSpans(s string) [][2]int {
	var spans [][2]int
	shadow := s
	add := func(start, end int) { spans = append(spans, [2]int{start, end}) }
	hide := func(from int) {
		if from == len(spans) {
			return
		}
		b := []byte(shadow)
		for _, span := range spans[from:] {
			for i := span[0]; i < span[1]; i++ {
				b[i] = ' '
			}
		}
		shadow = string(b)
	}
	for i, re := range []*regexp.Regexp{pemPrivateKey(), jwtToken()} {
		if !strings.Contains(shadow, []string{"-----BEGIN ", "eyJ"}[i]) {
			continue
		}
		from := len(spans)
		for _, loc := range re.FindAllStringIndex(shadow, -1) {
			add(loc[0], loc[1])
		}
		hide(from)
	}
	from := len(spans)
	if strings.Contains(strings.ToLower(shadow), "bearer") {
		for _, loc := range bearerAuth().FindAllStringSubmatchIndex(shadow, -1) {
			add(loc[3], loc[1])
		}
	}
	hide(from)
	starts := credentialStarts()
	for i := 0; i < len(shadow); {
		j := i
		for j < len(shadow) && tokenByte(shadow[j]) {
			j++
		}
		if j > i && starts[shadow[i]] && fullCredential(shadow[i:j]) {
			add(i, j)
		}
		i = max(j, i+1)
	}
	return mergeSecretSpans(spans)
}

// fullCredential is looksSecretToken minus the bare GitHub and Slack prefix checks, which match prose like "ghp_".
func fullCredential(run string) bool {
	return !notCredentialShaped(run) && startsCredential(run)
}

func mergeSecretSpans(spans [][2]int) [][2]int {
	slices.SortFunc(spans, func(a, b [2]int) int { return a[0] - b[0] })
	merged := spans[:0]
	for _, span := range spans {
		if n := len(merged); n > 0 && span[0] < merged[n-1][1] {
			merged[n-1][1] = max(merged[n-1][1], span[1])
		} else {
			merged = append(merged, span)
		}
	}
	return merged
}

// Derive the cheap rejection filter from the shared pattern so the prefix list cannot drift.
var credentialStarts = sync.OnceValue(func() [256]bool {
	re, err := syntax.Parse(CredentialRE().String(), syntax.Perl)
	if err != nil {
		panic("invalid credential pattern")
	}
	var starts [256]bool
	var visit func(*syntax.Regexp) bool
	visit = func(r *syntax.Regexp) bool {
		switch r.Op {
		case syntax.OpLiteral:
			if len(r.Rune) > 0 && r.Rune[0] < 256 {
				starts[r.Rune[0]] = true
			}
			return false
		case syntax.OpCharClass:
			for i := 0; i < len(r.Rune); i += 2 {
				for c := r.Rune[i]; c <= min(r.Rune[i+1], 255); c++ {
					starts[c] = true
				}
			}
			return false
		case syntax.OpConcat:
			for _, sub := range r.Sub {
				if !visit(sub) {
					return false
				}
			}
			return true
		case syntax.OpAlternate:
			nullable := false
			for _, sub := range r.Sub {
				nullable = visit(sub) || nullable
			}
			return nullable
		case syntax.OpCapture, syntax.OpPlus, syntax.OpRepeat:
			return visit(r.Sub[0])
		case syntax.OpQuest, syntax.OpStar:
			visit(r.Sub[0])
			return true
		default:
			return true
		}
	}
	visit(re)
	return starts
})
