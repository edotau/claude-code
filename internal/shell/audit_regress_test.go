package shell

import (
	"reflect"
	"slices"
	"testing"
)

// TestAliasVerbCaseInsensitive pins that git's case-insensitive alias names expand to the real verb.
func TestAliasVerbCaseInsensitive(t *testing.T) {
	if got := collectGit("git -c alias.xx=reset XX --hard", "/w"); !reflect.DeepEqual(got, []gitCall{{"/w", "reset"}}) {
		t.Errorf("alias XX = %v, want reset", got)
	}
	if !GitCommitInvocation("git -c alias.ci=commit CI --allow-empty -m x") {
		t.Error("upper-case alias of commit not seen as a commit")
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
		if !GitCommitInvocation(in) {
			t.Errorf("GitCommitInvocation(%q) = false", in)
		}
	}
}

// TestCommitAttachedMessageEndingInM pins that -mfoom is a whole message so a following --amend is still refused.
func TestCommitAttachedMessageEndingInM(t *testing.T) {
	for _, c := range []struct {
		args []string
		want bool
	}{
		{[]string{"-mfoom", "--amend"}, false},
		{[]string{"-m", "x", "--amend"}, false},
		{[]string{"-am", "x"}, true},
		{[]string{"--reuse-message=HEAD~3"}, false},
		{[]string{"--reu=HEAD"}, false},
		{[]string{"--reedit-message", "HEAD"}, false},
	} {
		if got := gitCommitAllowed(c.args, "."); got != c.want {
			t.Errorf("gitCommitAllowed(%q) = %v, want %v", c.args, got, c.want)
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

// TestReadOnlyGitRefusesNoIndex pins that --no-index (reads outside the repo) is refused for diff and grep.
func TestReadOnlyGitRefusesNoIndex(t *testing.T) {
	for _, cmd := range []string{
		"git diff --no-ext-diff --no-textconv --no-index /etc/passwd /dev/null",
		"git grep --no-index -e secret /etc",
	} {
		if ReadOnlyGit(cmd) {
			t.Errorf("ReadOnlyGit(%q) = true", cmd)
		}
	}
}
