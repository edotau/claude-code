package shell

import (
	"reflect"
	"testing"
)

type gitCall struct{ target, verb string }

func collectGit(cmd, cwd string) []gitCall {
	var out []gitCall
	EachGit(cmd, cwd, func(target, verb string, _ []string) bool {
		out = append(out, gitCall{target, verb})
		return false
	})
	return out
}

func TestEachGit(t *testing.T) {
	cases := []struct {
		cmd  string
		want []gitCall
	}{
		{"git stash", []gitCall{{"/w", "stash"}}},
		{"/usr/bin/git -C /r status", []gitCall{{"/r", "status"}}},
		{"cd /a && git add x; git commit", []gitCall{{"/a", "add"}, {"/a", "commit"}}},
		{"if cd /a; then git stash; fi", []gitCall{{"/a", "stash"}}},
		{"CDPATH=x cd -P -- /a && git stash", []gitCall{{"/a", "stash"}}},
		{"! git stash", []gitCall{{"/w", "stash"}}},
		{"env -C /a git stash && git status", []gitCall{{"/a", "stash"}, {"/w", "status"}}},
		{"sudo --chdir=/a git stash", []gitCall{{"/a", "stash"}}},
		{"sudo -D /a git stash", []gitCall{{"/a", "stash"}}},
		{"sudo --user root bash -c 'git stash'", []gitCall{{"/w", "stash"}}},
		{"timeout -s KILL 5 bash -c 'git stash'", []gitCall{{"/w", "stash"}}},
		{"timeout --kill-after=1 5 git stash", []gitCall{{"/w", "stash"}}},
		{"exec -a x bash -c 'git stash'", []gitCall{{"/w", "stash"}}},
		{`env -S "bash -c 'git stash'"`, []gitCall{{"/w", "stash"}}},
		{`env -C /a -S 'git log'`, []gitCall{{"/a", "log"}}},
		{"cd /a && bash -c 'cd b && git stash'", []gitCall{{"/a/b", "stash"}}},
		{"echo $(git commit -m x) `git add y`", []gitCall{{"/w", "commit"}, {"/w", "add"}}},
		{"eval 'git stash'", []gitCall{{"/w", "stash"}}},
		{"ssh host git stash", nil},
	}
	for _, c := range cases {
		if got := collectGit(c.cmd, "/w"); !reflect.DeepEqual(got, c.want) {
			t.Errorf("EachGit(%q) = %v, want %v", c.cmd, got, c.want)
		}
	}
}

func TestEachGitStopsAndBoundsNesting(t *testing.T) {
	n := 0
	if !EachGit("git a; git b", "/w", func(string, string, []string) bool { n++; return true }) || n != 1 {
		t.Errorf("fn returning true must stop the walk: calls=%d", n)
	}
	deep := "git stash"
	for range 8 {
		deep = "eval " + deep
	}
	if got, want := collectGit(deep, "/w"), []gitCall{{"/w", Uninspectable}}; !reflect.DeepEqual(got, want) {
		t.Errorf("nesting beyond maxNest must report Uninspectable, got %v", got)
	}
	if got := collectGit("eval eval eval git stash", "/w"); !reflect.DeepEqual(got, []gitCall{{"/w", "stash"}}) {
		t.Errorf("3-deep nesting stays inspectable, got %v", got)
	}
}

func TestEachGitCdOnlyMovesShellForBuiltins(t *testing.T) {
	for cmd, want := range map[string]string{
		"env cd /a; git x": "/w", "nohup cd /a; git x": "/w", "sudo cd /a; git x": "/w", "timeout 5 cd /a; git x": "/w",
		"nice cd /a; git x": "/w", "exec cd /a; git x": "/w",
		"command cd /a; git x": "/a", "time cd /a; git x": "/a", "builtin cd /a; git x": "/a",
		"FOO=1 cd /a; git x": "/a", "cd /a && git x": "/a", "env -C /a git x; git y": "/a",
	} {
		got := collectGit(cmd, "/w")
		if len(got) == 0 || got[0].target != want {
			t.Errorf("EachGit(%q) = %v, want first target %s", cmd, got, want)
		}
	}
}

func TestUnwrapDir(t *testing.T) {
	rest, dir := Unwrap([]string{"env", "-C", "/a", "FOO=1", "sudo", "-D", "b", "git", "x"}, "/w")
	if !reflect.DeepEqual(rest, []string{"git", "x"}) || dir != "/a/b" {
		t.Errorf("Unwrap = %v, %q", rest, dir)
	}
}

func TestCdTarget(t *testing.T) {
	if got, ok := CdTarget([]string{"cd", "-P", "--", "/a"}, "/w"); !ok || got != "/a" {
		t.Errorf("CdTarget = %q, %v", got, ok)
	}
	if got, ok := CdTarget([]string{"git"}, "/w"); ok || got != "/w" {
		t.Errorf("non-cd = %q, %v", got, ok)
	}
}

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
