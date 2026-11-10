package textutil

import (
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestRedact(t *testing.T) {
	tok := "ghp_" + strings.Repeat("a", 36)
	tok2 := "sk-" + strings.Repeat("b", 20)
	cases := []struct{ name, in, want string }{
		{"clean unchanged", "plain  text\twith\nspace", "plain  text\twith\nspace"},
		{"token replaced", "use " + tok + " now", "use REDACTED now"},
		{"multiple tokens", tok + " and " + tok2 + "\nend", "REDACTED and REDACTED\nend"},
		{"empty", "", ""},
		{"env var secret embedded", "export AWS_SECRET=AKIAIOSFODNN7EXAMPLE", "export AWS_SECRET=REDACTED"},
		{"json api key embedded", `{"api_key":"sk-abcdefghijklmnopqrstuvwxyz0123456789"}`, `{"api_key":"REDACTED"}`},
		{"bearer token embedded", "Authorization: Bearer " + strings.Repeat("a", 40), "Authorization: Bearer REDACTED"},
		{"multiline preserves newlines", "line1\nexport AWS_SECRET=AKIAIOSFODNN7EXAMPLE\nline3", "line1\nexport AWS_SECRET=REDACTED\nline3"},
		{"non-secret hyphenated text unchanged", "my-feature_flag is-enabled for_user", "my-feature_flag is-enabled for_user"},
		// replacement is positional (only the matched run), not a global substring ReplaceAll:
		// "risk-free" contains the literal substring "sk-" but must survive untouched.
		{"replacement is positional not substring-global", "sk- risk-free", "REDACTED risk-free"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := RedactSecrets(c.in); got != c.want {
				t.Errorf("RedactSecrets(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestRedactBearerPrefixedTokenSuffix(t *testing.T) {
	in := "Bearer sk-" + strings.Repeat("a", 40) + ".secret"
	if got := RedactSecrets(in); got != "Bearer REDACTED" {
		t.Errorf("RedactSecrets(%q) = %q, want full credential redacted as %q", in, got, "Bearer REDACTED")
	}
}

// Every GitHub token family is redacted, bare and embedded in prose or JSON.
func TestRedactGitHubTokenFamilies(t *testing.T) {
	for _, prefix := range []string{"ghu_", "gho_", "ghs_", "ghr_", "ghp_", "github_pat_"} {
		t.Run(prefix, func(t *testing.T) {
			tok := prefix + strings.Repeat("a1", 18)
			for _, in := range []string{tok, "token " + tok + " end", `{"t":"` + tok + `"}`} {
				got := RedactSecrets(in)
				if strings.Contains(got, tok) || !strings.Contains(got, "REDACTED") {
					t.Errorf("RedactSecrets(%q) = %q, token survived", in, got)
				}
			}
		})
	}
}

// GitHubPattern matches each family at minLen body characters and not below it.
func TestGitHubPatternMinLen(t *testing.T) {
	const minLen = 10
	re := regexp.MustCompile(GitHubPattern(minLen))
	for _, p := range GitHubPrefixes {
		if !re.MatchString(p + strings.Repeat("a", minLen)) {
			t.Errorf("%s at minLen must match", p)
		}
		if !re.MatchString(p + strings.Repeat("a_", minLen)) {
			t.Errorf("%s with underscores in the body must match", p)
		}
		if re.MatchString(p + strings.Repeat("a", minLen-1)) {
			t.Errorf("%s below minLen must not match", p)
		}
	}
	if re.MatchString("gha_" + strings.Repeat("a", 20)) {
		t.Error("unknown prefix must not match")
	}
	if !HasGitHubPrefix("ghu_x") || HasGitHubPrefix("gha_x") {
		t.Error("HasGitHubPrefix wrong")
	}
}

func TestPreview(t *testing.T) {
	cases := []struct {
		name, in string
		n        int
		want     string
	}{
		{"collapses whitespace", "a  b\n\tc   d", 100, "a b c d"},
		{"redacts credential word", "use sk-abcdef123456 now", 100, "use REDACTED now"},
		{"short input unchanged", "hello world", 11, "hello world"},
		{"n above length unchanged", "hello", 50, "hello"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := PreviewRedacted(c.in, c.n); got != c.want {
				t.Errorf("PreviewRedacted(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
			}
		})
	}
}

func TestPreviewTruncatesOnRuneBoundary(t *testing.T) {
	// Given a prompt of 4-byte runes longer than n; When truncated; Then the result is valid UTF-8 and ends in "…"
	got := PreviewRedacted(strings.Repeat("😀", 30), 10)
	if !utf8.ValidString(got) {
		t.Fatalf("PreviewRedacted split a rune: %q", got)
	}
	if !strings.HasSuffix(got, "…") || utf8.RuneCountInString(got) != 10 {
		t.Errorf("PreviewRedacted = %q (%d runes), want 10 runes ending in …", got, utf8.RuneCountInString(got))
	}
}

func TestPreviewRedactsThenTruncates(t *testing.T) {
	tok := "ghp_" + strings.Repeat("a", 36)
	got := PreviewRedacted("tok "+tok+" tail words here", 12)
	if want := "tok REDACTE…"; got != want { // redacted first (no ghp_), then cut to 12 runes
		t.Errorf("PreviewRedacted = %q, want %q", got, want)
	}
}

func TestLooksPlaceholderOnlyCountsInHead(t *testing.T) {
	// Given real tokens with "xxx" past the placeholder window; When checked; Then they are credential-shaped
	tok := "sk-Ab3K9fQ2LmZ7pR1tVw8xxxYc4Hd6Ns0J"
	if !LooksSecret(tok) {
		t.Errorf("LooksSecret(%q) = false, want true", tok)
	}
	if got := RedactSecrets("key=" + tok); strings.Contains(got, tok) {
		t.Errorf("RedactSecrets left token unredacted: %q", got)
	}
	// And a placeholder inside the head is still ignored
	if LooksSecret("sk-xxxxxxxxxxxxxxxxxxxxxxxx") {
		t.Error("head placeholder flagged as credential")
	}
}
