package shapes

import (
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
