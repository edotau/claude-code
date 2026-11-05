package shell

import (
	"slices"
	"testing"
)

// TestTokensPinnedOutput pins Tokens on quotes, escapes, group boundaries and line continuations.
func TestTokensPinnedOutput(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{`git log -1`, []string{"git", "log", "-1"}},
		{`  a   b  `, []string{"a", "b"}},
		{`echo "a b" 'c d'`, []string{"echo", "a b", "c d"}},
		{`echo ""`, []string{"echo", ""}},
		{`echo a""b`, []string{"echo", "ab"}},
		{`echo a\ b`, []string{"echo", "a b"}},
		{`echo "a\"b" "c\nd" "e\\f"`, []string{"echo", `a"b`, `c\nd`, `e\f`}},
		{`echo 'a\b'`, []string{"echo", `a\b`}},
		{`echo "unclosed arg`, []string{"echo", "unclosed arg"}},
		{`echo trailing\`, []string{"echo", `trailing\`}},
		{"echo a\\\nb", []string{"echo", "a\nb"}},
		{"a\tb\r\nc", []string{"a", "b", "c"}},
		{`(cd x)`, []string{"cd", "x"}},
		{`{ cd x; }`, []string{"cd", "x;"}},
		{`(cd x) y`, []string{"cd", "x", "y"}},
		{`((a))`, []string{"a"}},
		{`( a`, []string{"a"}},
		{`a)b`, []string{"a", "b"}},
		{`a}b`, []string{"a", "b"}},
		{`"a)b"`, []string{"a)b"}},
		{`(`, nil},
		{``, nil},
	}
	for _, c := range cases {
		if got := Tokens(c.in); !slices.Equal(got, c.want) {
			t.Errorf("Tokens(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
