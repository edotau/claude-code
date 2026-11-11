package shell

import (
	"reflect"
	"slices"
	"testing"
)

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
		if !gitCommitInvocation(in) {
			t.Errorf("gitCommitInvocation(%q) = false", in)
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

// TestBraceExpansionEndNestingAndQuotes pins that ${...} closes at its matching brace, past nested ${} and quoted braces.
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
