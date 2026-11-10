package shell

import (
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// TestMain treats every path as an existing directory unless it contains "missing"; statDir has its own test.
func TestMain(m *testing.M) {
	dirExists = func(p string) bool { return !strings.Contains(p, "missing") }
	os.Exit(m.Run())
}

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
		{"echo a\\\nb", []string{"echo", "ab"}}, // backslash-newline is removed, so the words join (POSIX sh)
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

func TestUnwrapNewWrappers(t *testing.T) {
	rest, _ := Unwrap([]string{"setsid", "stdbuf", "-oL", "ionice", "-c", "3", "chrt", "-f", "10", "git", "x"}, "/w")
	if !reflect.DeepEqual(rest, []string{"git", "x"}) {
		t.Errorf("Unwrap = %v", rest)
	}
}

func TestUnwrapAttachedShortValues(t *testing.T) {
	for _, in := range [][]string{{"env", "-C/a", "git", "x"}, {"sudo", "-D/a", "git", "x"}} {
		rest, dir := Unwrap(in, "/w")
		if !reflect.DeepEqual(rest, []string{"git", "x"}) || dir != "/a" {
			t.Errorf("Unwrap(%v) = %v, %q", in, rest, dir)
		}
	}
}
