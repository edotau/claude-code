package agent

import (
	"encoding/json"
	"testing"
)

// tomlString must take the quoted value only: a trailing comment or literal quotes must not ride into the model id.
func TestTomlString(t *testing.T) {
	cases := map[string]string{
		`"gpt-6-astra"`:             "gpt-6-astra",
		`"gpt-6-astra" # preferred`: "gpt-6-astra",
		`'gpt-6-astra'`:             "gpt-6-astra",
		`gpt-6-astra # bare`:        "gpt-6-astra",
		`"unterminated`:             "unterminated",
	}
	for in, want := range cases {
		if got := tomlString(in); got != want {
			t.Errorf("tomlString(%q) = %q, want %q", in, got, want)
		}
	}
}

// TOMLQuote must stay byte-identical to the json.Marshal-based quoter agent used before the merge.
func TestTOMLQuoteMatchesLegacyQuoter(t *testing.T) {
	legacy := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	for _, s := range []string{"<", ">", "&", "héllo 日本", "😀", `"q"`, `a\b`, "a\nb", "a\tb", "", "  \x00"} {
		if got, want := TOMLQuote(s), legacy(s); got != want {
			t.Errorf("TOMLQuote(%q) = %s, want %s", s, got, want)
		}
	}
}
