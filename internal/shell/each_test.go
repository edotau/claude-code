package shell

import (
	"reflect"
	"slices"
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
		{"git --super-prefix x/ stash", []gitCall{{"/w", "stash"}}},
		{"git --config-env core.x=HOME stash", []gitCall{{"/w", "stash"}}},
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
	EachGit(deep, "/w", func(_, _ string, args []string) bool {
		if !TooDeep(args) {
			t.Errorf("a call past maxNest must carry the too-deep marker, got %v", args)
		}
		return false
	})
	EachGit("v=x; git $v .", "/w", func(_, _ string, args []string) bool {
		if TooDeep(args) {
			t.Errorf("a shell-expanded verb is not nesting too deep")
		}
		return false
	})
	if got := collectGit("eval eval eval git stash", "/w"); !reflect.DeepEqual(got, []gitCall{{"/w", "stash"}}) {
		t.Errorf("3-deep nesting stays inspectable, got %v", got)
	}
}

// TestEachGitCasePatternsAreNotPipes: `in sh|bash) …` is a pattern list; a real pipe into a shell is still reported.
func TestEachGitCasePatternsAreNotPipes(t *testing.T) {
	for _, cmd := range []string{"case $c in sh|bash) p=1;; *) break;; esac", "case $c in\n a|sh|bash) p=1;;\n -zsh|dash) p=2;; esac"} {
		if got := collectGit(cmd, "/w"); len(got) != 0 {
			t.Errorf("EachGit(%q) = %v, want no calls", cmd, got)
		}
	}
	for _, cmd := range []string{"(cat f | bash)", "echo x; case $c in a) cat f | bash;; esac"} {
		if got := collectGit(cmd, "/w"); len(got) == 0 {
			t.Errorf("EachGit(%q) saw nothing, want the pipe into a shell reported", cmd)
		}
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
		{"dynamic cd target is unknown in its && chain", `cd "$R" && git x`, unknownDir},
		{"substituted cd target is unknown", "cd $(mktemp -d) && git x", unknownDir},
		{"pipe in the && chain after a dynamic cd", `cd "$R" && ls | git x`, unknownDir},
		{"relative -C cannot climb out of an unknown dir", `cd "$R" && git -C ../../w x`, unknownDir},
		{"missing target is where the && chain runs", "cd /missing && git x", "/missing"},
		{"dynamic cd then ; keeps the old dir", `cd "$R"; git x`, "/w"},
		{"dynamic cd then || keeps the old dir", `cd "$R" || git x`, "/w"},
		{"dynamic cd, later || keeps the old dir", `cd "$R" && true || git x`, "/w"},
		{"dynamic cd, newline keeps the old dir", "cd \"$R\"\ngit x", "/w"},
		{"missing cd then a later ; keeps the old dir", "cd /missing && true; git x", "/w"},
		{"dynamic cd, subshell, then ;", `cd "$R" && (true); git x`, "/w"},
		{"literal cd after a dynamic one", `cd "$R" && cd /a && git x`, "/a"},
		{"explicit -C after a dynamic cd", `cd "$R" && git -C /a x`, "/a"},
		{"~user names another home", "cd ~root && git x", unknownDir},
		{"cd - goes back to the previous dir", "cd /a; cd - && git x", "/w"},
		{"cd - with no previous dir is the starting dir", "cd - && git x", "/w"},
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
	for _, cmd := range []string{"claude-code doctor git x", "claude-code --host h git x", "claude-code memory update git x",
		"claude-code memory recall y && git x", "echo git x | sh", "claude-code memory recall $(git x)",
		"$(echo git x)", "`echo git x`", `eval "$(echo git x)"`, "bash <(echo git x)", ". <(echo git x)",
		"echo git x | xargs -I{} sh -c {}", "echo git x > /tmp/s.sh; sh /tmp/s.sh"} { // echo output that runs
		if got := collectGit(cmd, "/w"); len(got) != 1 || got[0] != (gitCall{"/w", "x"}) {
			t.Errorf("EachGit(%q) = %v, want one x call", cmd, got)
		}
	}
	for _, cmd := range []string{"echo 'git x'", "git commit -m 'git y fix'", "xargs echo", "echo hi | bash -c 'cat'", "echo hi | bash",
		"echo run git x later", "grep -rn git x internal", "claude-code memory recall git x", "claude-code memory recall 'git x'",
		"claude-code network websearch --query 'git x vs git y'", "claude-code scm pr create --title 'git x guard'",
		"claude-code memory remember 'never git x'", "claude-code scm comment 1 git x"} {
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
		"git --config-env alias.st=V st":          Uninspectable,
		`printf 'git x\n' > f && sh f`:            "x",
		"echo 'git x' | tee f && bash f":          "x",
	}
	for cmd, want := range cases {
		got := collectGit(cmd, "/w")
		if len(got) == 0 || got[len(got)-1].verb != want {
			t.Errorf("EachGit(%q) = %v, want verb %s", cmd, got, want)
		}
	}
}

func TestEachGitWorktreeCommitNeedsCommandWord(t *testing.T) {
	ph := "claude-code worktree commit"
	text := []string{
		"grep -rn '" + ph + "' rules", "rg '" + ph + "' rules", "rtk grep -rn \"" + ph + "\" rules", "rtk rg '" + ph + "' .",
		"rtk read '" + ph + "'", "rtk find . -name '" + ph + "'", "rtk ls '" + ph + "'", "echo " + ph, "printf '%s' '" + ph + "'",
		"git log --grep='" + ph + "'", "rtk git log --grep=\"" + ph + "\"", "git commit -m \"fix: " + ph + " verb\"",
		"cat <<'EOF'\n" + ph + " -m x\nEOF", "ls # " + ph, "ls '" + ph + "' dir", "xargs grep " + ph,
	}
	for _, cmd := range text {
		for _, c := range collectGit(cmd, "/w") {
			if c.verb == WorktreeCommit {
				t.Errorf("EachGit(%q) emitted the verb for text", cmd)
			}
		}
	}
	real := []string{
		ph + " -m x f", "/usr/local/bin/" + ph, "rtk " + ph, "env -i " + ph, "sudo -u me " + ph, "nohup " + ph, "time " + ph,
		"command " + ph, "go run ./cmd/" + ph, "bash -c '" + ph + "'", "sh -c \"" + ph + "\"", "eval '" + ph + "'",
		"(" + ph + ")", "echo $(" + ph + ")", "xargs -n 1 " + ph, "watch '" + ph + "'", "watch -n1 " + ph,
		"find . -exec " + ph + " {} \\;", "ls && " + ph,
	}
	for _, cmd := range real {
		if !slices.ContainsFunc(collectGit(cmd, "/w"), func(c gitCall) bool { return c.verb == WorktreeCommit }) {
			t.Errorf("EachGit(%q) lost the worktree commit verb", cmd)
		}
	}
}
