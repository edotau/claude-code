package agent

import "testing"

type roCase struct {
	cmd  string
	want bool
}

func checkReadOnly(t *testing.T, cases []roCase) {
	t.Helper()
	for _, c := range cases {
		if got := ReadOnlyShell(c.cmd); got != c.want {
			t.Errorf("ReadOnlyShell(%q) = %v, want %v", c.cmd, got, c.want)
		}
	}
}

func TestReadOnlyGitForms(t *testing.T) {
	checkReadOnly(t, []roCase{
		{"git status", true},
		{"git log --no-ext-diff --no-textconv -n 5", true},
		{"git ls-files internal", true},
		{"git -c core.pager=x log", false},
		{"git log --output=x", false},
		{"git commit -m x", false},
		{"git diff", true},
		{"git log -3 --oneline | head -2", true},
		{"git show HEAD~1:internal/x.go", true},
		{"git blame -L 1,5 f.go", true},
		{"git branch --list", true},
		{"git branch -D x", false},
		{"git branch new", false},
		{"git diff --ext-diff", true},
		{"GIT_EXTERNAL_DIFF=x git diff", false},
		{"X=--output=f git log $X", false},
		{"git --config-env=a=B log", false},
		{"git -C /tmp log", false},
		{"git log --exec-path=/x", false},
		{"git log --out=x", false},
		{"git grep -O foo", false},
		{"git diff --no-index /etc/passwd x", false},
		{"git diff /etc/passwd /etc/hosts", false},
		{"git diff ~root", false},
		{"git log --pager=x", false},
	})
}

func TestReadOnlyTools(t *testing.T) {
	checkReadOnly(t, []roCase{
		{"claude-code agents diff", true},
		{"claude-code agents diff --verify x", true},
		{"claude-code agents diff --check", false},
		{"claude-code agents diff -check", false},
		{"claude-code agents find foo", true},
		{"claude-code memory recall x | head -5", true},
		{"claude-code network websearch --query q", true},
		{"claude-code agents ask x", false},
		{"claude-code memory timeline --write", false},
		{"claude-code gateway --overlay {}", false},
		{"go doc fmt.Println", true},
		{"go list -f {{.Dir}} ./...", true},
		{"go list -toolexec x ./...", false},
		{"go env GOPATH", true},
		{"go env -w GOFLAGS=x", false},
		{"go build ./...", false},
	})
}

func TestReadOnlyReviewScope(t *testing.T) {
	checkReadOnly(t, []roCase{
		{"claude-code review scope --range HEAD~1..HEAD --caps", true},
		{"claude-code review scope --paths internal/agent,cmd --importers --format json", true},
		{"claude-code review scope --range HEAD^..HEAD | head -5", true},
		{"claude-code review scope --range --output=x", false},
		{"claude-code review scope --range -x", false},
		{"claude-code review scope --paths /etc", false},
		{"claude-code review scope --paths a,../b", false},
		{"claude-code review scope --repo /tmp/x --caps", false},
		{"claude-code review scope --format sarif", false},
		{"claude-code review scope --out x", false},
		{"claude-code review scope --range 'a;b'", false},
		{"claude-code memory report --label x", false},
	})
}

func TestReadOnlyShell(t *testing.T) {
	checkReadOnly(t, []roCase{
		{"rg foo internal && ls", true},
		{"rg --pre cat foo", false},
		{`rg "->" internal`, true},
		{"cat a > b", false},
		{"cat a 2>&1", true},
		{"cat <<EOF", false},
		{"cat $(rm x)", false},
		{"cat $(ls)", false},
		{"cat `ls`", false},
		{"ls; rm x", false},
		{"ls | tee x", false},
		{"sed -n 1,5p file.go", true},
		{"sed -i s/a/b/ file", false},
		{"tail -f log", false},
		{"time git status", true},
		{"FOO=1 git status", true},
		{"PATH=/x git status", false},
		{"bash -c 'ls'", false},
		{"mytool --help", true},
		{"mytool --version", true},
		{"./mytool --help", false},
		{"rm --help x", false},
		{"", false}, {"  \t ", false}, {"ls #\nrm x", false}, {"ls\nrm x", false},
		{"ls && git commit -m x", false}, {"ls || rm x", false}, {"ls && cat a > b", false}, {"cat a | sh", false},
		{"cat a | xargs rm", false}, {"cat a | sed -i s/a/b/ f", false}, {"ls & rm x", false},
	})
}

// With a Read deny present, a read-only shell command may not reach the denied path, or must be listable to prove it.
func TestReadOnlyShellHonorsReadDeny(t *testing.T) {
	p := Permissions{Allow: []Rule{ReadOnlyAllow}, Deny: append([]Rule{{Tool: "Read", Arg: ".env"}}, ReadOnlyDeny...)}
	for cmd, want := range map[string]Decision{
		"cat README.md": Allow, "cat .env": Deny, "cat sub/.env": Deny, "head -3 ./.env": Deny, "cat *.env": Deny,
		"cat $HOME/x": Deny, "wc -l a.go b.go": Allow, "git log -1": Allow, "git status": Allow, "git diff": Deny,
		"git show HEAD:.env": Deny, "git log -p": Deny, "rg foo": Deny, "grep -r foo .": Deny, "grep foo a.go": Allow,
		"sed -n 1,3p .env": Deny, "claude-code agents find x": Deny, "claude-code memory recall x": Allow,
		"go doc fmt": Allow, "git log --stat | head -3": Allow,
	} {
		if got := p.Decide("Bash", cmd); got != want {
			t.Errorf("%q = %v, want %v", cmd, got, want)
		}
	}
	if got := (Permissions{Allow: []Rule{ReadOnlyAllow}, Deny: ReadOnlyDeny}).Decide("Bash", "rg foo"); got != Allow {
		t.Errorf("rg with no Read deny = %v, want Allow", got)
	}
}

func TestReadOnlyBeatsAllow(t *testing.T) {
	p := Permissions{Allow: []Rule{{Tool: "Bash"}, ReadOnlyAllow}, Deny: ReadOnlyDeny}
	if got := p.Decide("Bash", "rm x"); got != Deny {
		t.Errorf("rm under a blanket allow = %v, want Deny", got)
	}
	if got := p.Decide("Bash", "ls"); got != Allow {
		t.Errorf("ls = %v, want Allow", got)
	}
	if got := p.Decide("Write", "x"); got != Deny {
		t.Errorf("Write = %v, want Deny", got)
	}
}

func TestParseRuleExpandsHome(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	for rule, want := range map[string]string{"Edit(~/a/**)": "Edit(/home/u/a/**)", "Bash(~/x)": "Bash(~/x)", "Read(a/~/b)": "Read(a/~/b)",
		"Read(~)": "Read(~)", "Read(~root/x)": "Read(~root/x)", "WebFetch(~/x)": "WebFetch(~/x)"} {
		r, err := ParseRule(rule)
		if err != nil || r.String() != want {
			t.Errorf("ParseRule(%q) = %v, %v; want %s", rule, r, err, want)
		}
	}
	t.Setenv("HOME", "") // no home: the rule stays as written, never joined onto an empty home
	if r, err := ParseRule("Edit(~/a)"); err != nil || r.String() != "Edit(~/a)" {
		t.Errorf("ParseRule with no HOME = %v, %v; want Edit(~/a)", r, err)
	}
}

func TestReadOnlyAgentsWait(t *testing.T) {
	checkReadOnly(t, []roCase{
		{"claude-code agents wait job --timeout 10m", true},
		{"claude-code agents wait job --save /tmp/result", false},
		{"claude-code agents dispatch --waves plan --background", false},
	})
}

func TestReadOnlyAgentsMergedVerbs(t *testing.T) {
	checkReadOnly(t, []roCase{
		{"claude-code agents result -f job", true},
		{"claude-code agents result job --events -f", true},
		{"claude-code agents result job --agent codex", true},
		{"claude-code agents status", true},
		{"claude-code agents status job --agent codex --all", true},
		{"claude-code agents result job --save /tmp/x", false},
		{"claude-code agents cancel job", false},
		{"claude-code agents cancel --lost", false},
		{"claude-code agents tail job", false},
		{"claude-code agents reap", false},
	})
}
