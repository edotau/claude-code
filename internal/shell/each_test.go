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
