package shell

import (
	"reflect"
	"testing"
)

// TestEachGitCwdScoping: a cd that does not move the real shell must not move the reported target.
func TestEachGitCwdScoping(t *testing.T) {
	cases := []struct{ name, cmd, want string }{
		{"subshell cd does not leak", "(cd /a); git x", "/w"},
		{"subshell spanning segments", "(cd /a && ls); git x", "/w"},
		{"cd inside the subshell applies there", "(cd /a && git x)", "/a"},
		{"|| after a cd keeps the old dir", "cd /a || git x", "/w"},
		{"&& after a cd uses the new dir", "cd /a && git x", "/a"},
		{"|| after a later command", "cd /a && true || git x", "/a"},
		{"missing target leaves cwd", "cd /missing; git x", "/w"},
		{"pipe left side is a subshell", "cd /a | git x", "/w"},
		{"pipe right side is a subshell", "ls | cd /a; git x", "/w"},
		{"backgrounded cd", "cd /a & git x", "/w"},
		{"pushd then popd", "pushd /a; popd; git x", "/w"},
		{"pushd stays until popd", "pushd /a; git x", "/a"},
		{"popd with an empty stack", "popd; git x", "/w"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := collectGit(c.cmd, "/w")
			if len(got) != 1 || got[0].target != c.want {
				t.Errorf("EachGit(%q) = %v, want one call at %s", c.cmd, got, c.want)
			}
		})
	}
}

func TestStatDir(t *testing.T) {
	if !statDir(t.TempDir()) || statDir("/nonexistent_zz/q") {
		t.Error("statDir must be true for a real directory and false for a missing one")
	}
}

// TestEachGitStringArgs: git inside a string argument of a wrapper, xargs/find/watch or a piped shell is found.
func TestEachGitStringArgs(t *testing.T) {
	for _, cmd := range []string{
		"setsid sh -c 'git x'", "find . -maxdepth 0 -exec sh -c 'git x' +", "xargs -I{} sh -c 'git x {}'",
		"watch 'git x'", "echo 'git x' | bash", "bash <<< 'git x'", "bash <<<'git x'", "su -c 'git x' root",
		"script -qc 'git x' /dev/null", "su -c 'ls; git x' root", "script -qc 'ls; git x' /dev/null", "stdbuf -oL sh -c 'git x'", "echo 'git x' | sudo bash -s",
	} {
		got := collectGit(cmd, "/w")
		if len(got) != 1 || got[0] != (gitCall{"/w", "x"}) {
			t.Errorf("EachGit(%q) = %v, want one x call", cmd, got)
		}
	}
	for _, cmd := range []string{"echo 'git x'", "git commit -m 'git y fix'", "xargs echo", "echo hi | bash -c 'cat'", "echo hi | bash"} {
		if got := collectGit(cmd, "/w"); len(got) > 1 || len(got) == 1 && got[0].verb != "commit" {
			t.Errorf("EachGit(%q) = %v, want no inspected git text", cmd, got)
		}
	}
	if got := collectGit("cat f | bash", "/w"); !reflect.DeepEqual(got, []gitCall{{"/w", Uninspectable}}) {
		t.Errorf("a pipe into a shell from an unseen source must be Uninspectable, got %v", got)
	}
}

// TestEachGitHiddenVerbs: an alias is expanded and a shell-expanded verb is Uninspectable.
func TestEachGitHiddenVerbs(t *testing.T) {
	cases := map[string]string{
		"git -c alias.zz=checkout zz .":           "checkout",
		"git -c alias.x='commit --amend' x":       "commit",
		"git -c alias.checkout=status checkout .": "checkout",
		"git -c alias.s='!sh' s":                  Uninspectable,
		"git $(echo checkout) .":                  Uninspectable,
		"git checkout{,} .":                       Uninspectable,
		"v=checkout; git $v .":                    Uninspectable,
		"git $'checkout' .":                       "checkout",
		"git $'check\\x6fut' .":                   "checkout",
		"git -c color.ui=always status":           "status",
	}
	for cmd, want := range cases {
		got := collectGit(cmd, "/w")
		if len(got) == 0 || got[len(got)-1].verb != want {
			t.Errorf("EachGit(%q) = %v, want verb %s", cmd, got, want)
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

func TestAttributionFooterHidden(t *testing.T) {
	for cmd, want := range map[string]bool{
		`git commit -m "x Co-Authored-By: Cl""aude"`:                        true,
		`git commit -m $'Co-Authored-By:\x20Claude'`:                        true,
		`git commit -m "Generated with [Claude](https://c.ai)"`:             true,
		`git commit -m "Co-Authored-By: Anthropic <a@b>"`:                   true,
		`git -c alias.ci=commit ci -m "Co-Authored-By: Claude"`:             true,
		`git $(echo commit) -m "Co-Authored-By: Claude"`:                    true,
		`setsid sh -c 'git commit -m "Co-Authored-By: Claude"'`:             true,
		`git commit -m "$(printf 'Co-Authored-By: %s' Claude)"`:             true,
		`git commit -m "mention co-authored-by docs"`:                       false,
		"git commit -m \"$(cat <<'EOF'\nfix: co-authored-by docs\nEOF\n)\"": false,
		`echo $(date); git commit -m "mention co-authored-by docs"`:         false,
		`git commit -m "fix: x"`:                                            false,
	} {
		if got := AttributionFooterCommit(cmd); got != want {
			t.Errorf("AttributionFooterCommit(%q) = %v, want %v", cmd, got, want)
		}
	}
}
