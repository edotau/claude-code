package shell

import (
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	_ "github.com/edotau/claude-code/internal/platform/nonet"
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
		{`>important.txt`, []string{">important.txt"}},
		{`> f`, []string{">", "f"}},
		{`>f echo hi`, []string{"echo", "hi"}},
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

// Given a leading keyword, redirect or function header, Then Unwrap/Tokens reach the command; echo data stays data.
func TestUnwrapLeadWords(t *testing.T) {
	for in, want := range map[string]string{
		"then curl x": "curl x", ">/dev/null curl x": "curl x", "> /dev/null 2>&1 curl x": "curl x",
		"f() { curl x": "curl x", "function f { curl x": "curl x", "do <in.txt curl x": "curl x",
		"! A=1 curl x": "curl x", "echo then curl x": "echo then curl x", "echo '>/dev/null curl x'": "echo >/dev/null curl x",
	} {
		rest, _ := Unwrap(Tokens(in), "")
		if got := strings.Join(rest, " "); got != want {
			t.Errorf("Unwrap(Tokens(%q)) = %q, want %q", in, got, want)
		}
	}
}

// Given `--` after -c or eval, Then the nested command is the string past it; echo data has none.
func TestNestedCommandDashDash(t *testing.T) {
	for in, want := range map[string]string{
		`sh -c -- "curl x"`: "curl x", `eval -- "curl x"`: "curl x", `bash -c "curl x"`: "curl x", `echo -c -- "curl x"`: "",
	} {
		if got, _ := NestedCommand(Tokens(in)); got != want {
			t.Errorf("NestedCommand(%q) = %q, want %q", in, got, want)
		}
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

// TestTokensGroupOpenerAfterControlWord pins that ( and { openers are stripped after !, then and nested openers.
func TestTokensGroupOpenerAfterControlWord(t *testing.T) {
	for in, want := range map[string][]string{
		"! (git commit -m x)":                 {"!", "git", "commit", "-m", "x"},
		"( ( git commit ) )":                  {"git", "commit"},
		"if true; then (git commit -m x); fi": nil,
	} {
		got := Tokens(in)
		if want != nil && !slices.Equal(got, want) {
			t.Errorf("Tokens(%q) = %q, want %q", in, got, want)
		}
	}
	for _, in := range []string{"! (git commit -m x)", "then { git commit -m x; }", "if true; then (git commit -m x); fi"} {
		if got := collectGit(in, "/w"); !reflect.DeepEqual(got, []gitCall{{"/w", "commit"}}) {
			t.Errorf("collectGit(%q) = %v, want one commit", in, got)
		}
	}
}

// TestUnwrapClusteredValueFlag pins that a short cluster ending in a value flag (-nu root) consumes its value.
func TestUnwrapClusteredValueFlag(t *testing.T) {
	for _, in := range [][]string{
		{"sudo", "-nu", "root", "git", "commit"}, {"sudo", "-Eu", "root", "git", "commit"}, {"env", "-iu", "X", "git", "commit"},
	} {
		if rest, _ := Unwrap(in, "/w"); !reflect.DeepEqual(rest, []string{"git", "commit"}) {
			t.Errorf("Unwrap(%v) = %v", in, rest)
		}
	}
}

// TestTokensKeepsBraceExpansionWhole pins that an unquoted ${VAR} stays one word while a bare } still splits.
func TestTokensKeepsBraceExpansionWhole(t *testing.T) {
	if got, want := Tokens("cd ${HOME}/repo"), []string{"cd", "${HOME}/repo"}; !slices.Equal(got, want) {
		t.Errorf("Tokens = %q, want %q", got, want)
	}
	if got, want := Tokens("{ cd x; }"), []string{"cd", "x;"}; !slices.Equal(got, want) {
		t.Errorf("group close = %q, want %q", got, want)
	}
}

// TestBraceExpansionEndNestingAndQuotes pins that ${...} closes at its matching brace, past nested and quoted braces.
func TestBraceExpansionEndNestingAndQuotes(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"echo ${x:-${y}} z", []string{"echo", "${x:-${y}}", "z"}},
		{`echo ${x:-"}"} z`, []string{"echo", `${x:-"}"}`, "z"}},
		{`echo ${x:-\}} z`, []string{"echo", `${x:-\}}`, "z"}},
		{"echo ${x:-unterminated z", []string{"echo", "${x:-unterminated", "z"}},
	}
	for _, c := range cases {
		if got := Tokens(c.in); !slices.Equal(got, c.want) {
			t.Errorf("Tokens(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func BenchmarkTokensSegments(b *testing.B) {
	const cmd = `git -C "/a b/c" log --oneline && echo 'x y' | grep -E "foo|bar" ; (cd ./dir && make test) || true`
	for range b.N {
		for _, seg := range Segments(cmd) {
			_ = Tokens(seg)
		}
	}
}

func BenchmarkTokensLongQuote(b *testing.B) {
	const seg = `git commit -m "perf(shell): Segments skips the separator list it throws away" -m 'body'`
	for range b.N {
		_ = Tokens(seg)
	}
}

func BenchmarkQuoteAt(b *testing.B) {
	cmd := strings.Repeat(`echo "a $x \"q\" b" 'lit ; |' plain && `, 8)
	for range b.N {
		_ = QuoteAt(cmd)
	}
}
