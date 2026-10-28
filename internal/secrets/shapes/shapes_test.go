package shapes

import (
	"regexp"
	"strings"
	"testing"
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
			if got := Redact(c.in); got != c.want {
				t.Errorf("Redact(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// Every GitHub token family is redacted, bare and embedded in prose or JSON.
func TestRedactGitHubTokenFamilies(t *testing.T) {
	for _, prefix := range []string{"ghu_", "gho_", "ghs_", "ghr_", "ghp_", "github_pat_"} {
		t.Run(prefix, func(t *testing.T) {
			tok := prefix + strings.Repeat("a1", 18)
			for _, in := range []string{tok, "token " + tok + " end", `{"t":"` + tok + `"}`} {
				got := Redact(in)
				if strings.Contains(got, tok) || !strings.Contains(got, "REDACTED") {
					t.Errorf("Redact(%q) = %q, token survived", in, got)
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
