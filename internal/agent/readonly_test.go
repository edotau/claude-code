package agent

import (
	"path/filepath"
	"testing"
)

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
		{"git diff", true},
		{"git log -3 --oneline | head -2", true},
		{"git show HEAD~1:internal/x.go", true},
		{"git blame -L 1,5 f.go", true},
		{"git branch --list", true},
		{"git diff --ext-diff", true},
		{"git diff --no-index a b", true},
		{"git -C /tmp log", true},
		{"git -C /abs/repo status --short", true},
		{"git -C /abs/repo status --short | wc -l", true},
		{"git -C /abs/repo log --oneline -5", true},
		{"git -C /abs/repo diff --stat", true},
		{"git -C /abs/repo diff --ext-diff", true},
		{"git -C ~/repo status", true},
		{"git -C ../sibling status", true},
		{"git -C a -C b status", true},
		{"git --no-pager -C d log --oneline -3", true},
	})
}

func TestReadOnlyGitRefused(t *testing.T) {
	cmds := []string{"git -c core.pager=x log", "git log --output=x", "git commit -m x", "git branch -D x", "git branch new",
		"GIT_EXTERNAL_DIFF=x git diff", "X=--output=f git log $X", "git --config-env=a=B log", "git log --exec-path=/x",
		"git log --out=x", "git grep -O foo", "git diff --no-index /etc/passwd x", "git diff /etc/passwd /etc/hosts",
		"git diff ~root", "git log --pager=x",
		"git -C /abs/repo -c core.pager=touch\\ /tmp/x log", "git -C /abs/repo diff --output=/tmp/x", "git -C /abs/repo branch new",
		"git -C /abs/repo stash", "git -C /abs/repo config user.name x", "GIT_PAGER=x git -C /abs/repo log",
		"git -C /etc diff passwd hosts", "git -C /etc diff --no-index passwd hosts", "git -C /abs/repo log -- /etc/passwd",
		"git -C /etc diff passwd -- hosts", "git -C /etc diff -- passwd hosts", "git -C /etc grep --no-index root",
		"git -C /etc grep --no-ind root", "git -C d --git-dir=/x status", "git -C d --work-tree=/x status",
		"git -C d --exec-path=/x status", "git --exec-path=/x -C d status", "git --no-pager --exec-path=/x status",
		"git -C d -p log", "git -C d format-patch HEAD~1", "git -C d bundle create /tmp/b HEAD", "git -C d archive -o /tmp/a.tar HEAD",
		"git -C d tag x", "git -C d worktree add /tmp/w", "git -C d gc", "git -C d fetch", "git -C d pull",
		"git -C d submodule update", "git -C d difftool", "git -C d grep -O x", "GIT_DIR=/x git -C d status",
		"GIT_EXTERNAL_DIFF=x git -C d diff", "GIT_CONFIG_COUNT=1 git -C d log", "git -C", "git -C=d status", "git -Cd status"}
	for _, c := range cmds {
		if ReadOnlyShell(c) {
			t.Errorf("ReadOnlyShell(%q) = true, want false", c)
		}
	}
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
		{"claude-code doctor -h", true},
		{"claude-code review simplify -h", true},
		{"claude-code review simplify --help", true},
		{"claude-code doctor --fix -h", false},
		{"claude-code doctor -h x", false},
		{"claude-code doctor", false},
		{"claude-code memory update --catchup", false},
		{"claude-code gateway --overlay {}", false},
		{"go doc fmt.Println", true},
		{"go list -f {{.Dir}} ./...", true},
		{"go list -toolexec x ./...", false},
		{"go env GOPATH", true},
		{"go env -w GOFLAGS=x", false},
		{"go build ./...", false},
	})
}

func TestReadOnlyRenamedVerbs(t *testing.T) {
	checkReadOnly(t, []roCase{
		{"claude-code plan status --all", true},
		{"claude-code plan assumptions plan.md", true},
		{"claude-code plan goals --json", true},
		{"claude-code plan tick --range a..b", false},
		{"claude-code review assumptions", false},
		{"claude-code review goals", false},
		{"claude-code models preflight", true},
		{"claude-code models preflight --refresh", false},
		{"claude-code models preflight --discover p", false},
		{"claude-code models preflight --live", false},
		{"claude-code review quality --comments --format tsv internal", true},
		{"claude-code review quality --comments --output x", false},
		{"claude-code review simplify --fail", true},
		{"claude-code gateway resolve codex", true},
		{"claude-code agents resolve codex", false},
		{"claude-code configure agents x profile=y", false},
		{"claude-code memory fmt --own", false},
		{"claude-code gateway proxy --restart", false},
		{"claude-code clip --clear", false},
		{"claude-code vault get TAVILY_API_KEY", true},
		{"claude-code vault get TAVILY_API_KEY >/dev/null 2>&1", true},
		{"claude-code vault get X | head -1", true},
		{"claude-code vault ls", true},
		{"claude-code vault set X", false},
		{"claude-code vault rm X", false},
		{"claude-code vault delete X", false},
		{"claude-code vault get --key", false},
		{"claude-code vault get --key | cat", false},
		{"rtk claude-code vault get --key", false},
		{"claude-code vault get -key", false},
		{"claude-code vault get --key=true", false},
		{"claude-code vault get NAME | wc -c", true},
		{"claude-code vault get NAME 2>/dev/null", true},
		{"claude-code vault get X > out.txt", false},
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
		{"cat a > /dev/null", true},
		{"cat a 2>/dev/null", true},
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

func TestReadOnlyRtkPrefix(t *testing.T) {
	checkReadOnly(t, []roCase{
		{"rtk git status", true}, {"rtk git -C /abs/repo status --short | wc -l", true},
		{"rtk git -C /abs/repo log --oneline -5", true}, {"rtk ls", true}, {"rtk grep -rn x .", true}, {"rtk rg x", true},
		{"rtk read f.go", true}, {"rtk read -n f.go", true}, {"rtk claude-code vault get NAME | wc -c", true},
		{"rtk find . -name y", false}, {"rtk go test ./...", false}, {"rtk make lint", false}, {"rtk proxy rm x", false},
		{"rtk run ls", false}, {"rtk rtk git status", false}, {"rtk git -C /abs/repo stash", false},
		{"rtk git diff --output=/tmp/x", false}, {"rtk git -c core.pager=x log", false}, {"rtk rg --pre cat x", false},
		{"rtk claude-code vault get --key", false}, {"rtk claude-code vault set X", false},
		{"rtk read /abs/file > /tmp/x", false}, {"rtk git -C /etc diff -- passwd hosts", false}, {"rtk", false}, {"rtk --help", true}, {"rtk -h", true}, {"git --help", true}, {"claude-code vault --help", true},
		{"rtk git --help", true}, {"rtk claude-code vault --help", true}, {"rtk git --help x", false},
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
		"rtk read .env": Deny, "rtk read README.md": Allow, "rtk git -C .env status": Deny, "rtk git -C d status": Allow,
	} {
		if got := p.Decide("Bash", cmd); got != want {
			t.Errorf("%q = %v, want %v", cmd, got, want)
		}
	}
	if got := (Permissions{Allow: []Rule{ReadOnlyAllow}, Deny: ReadOnlyDeny}).Decide("Bash", "rg foo"); got != Allow {
		t.Errorf("rg with no Read deny = %v, want Allow", got)
	}
}

// A Read deny refuses only segments that may read it: cd elsewhere and echo pass, a cd into or above it does not.
func TestReadOnlyReadDenySparesNoFileSegments(t *testing.T) {
	root := filepath.ToSlash(t.TempDir())
	store, secret := root+"/memory", root+"/state/vault.db"
	base := []Rule{ReadOnlyAllow, {Tool: "Bash", Arg: "echo *"}, {Tool: "Bash", Arg: "printf *"}, {Tool: "Bash", Arg: "pwd"},
		{Tool: "Bash", Arg: "true"}, CdRule("/")}
	rows := map[string][2]Decision{ // command: {with the Read deny, without it}
		"cd " + store + " && wc -l f && echo done":    {Allow, Allow},
		"cd " + store + " && pwd && true && printf x": {Allow, Allow},
		"cat " + secret:                                {Deny, Allow},
		"cd " + store + " && cat " + secret:            {Deny, Allow},
		"head -1 " + secret + " | wc -c":               {Deny, Allow},
		"cd " + root + "/state && cat vault.db":        {Deny, Allow},
		"cd " + root + " && cat state/vault.db":        {Deny, Allow},
		"cd " + store + " && cat ../state/vault.db":    {Deny, Allow},
		"git -C " + secret + " status":                 {Deny, Allow},
		"git -C " + store + " status":                  {Allow, Allow},
		"cd memory && cat f":                           {Deny, Deny},
		"cd $HOME && cat f":                            {Deny, Deny},
		"cat $(echo " + secret + ")":                   {Deny, Ask},
		"cat " + root + "/state/vault$(git log -0).db": {Deny, Ask},
		"cat " + root + "/state/vault$(echo).db":       {Deny, Ask},
		"wc -l `echo " + secret + "`":                  {Deny, Ask},
		"grep x " + secret:                             {Deny, Allow},
		"grep -v '^$' f && grep -c '[0-9]' f":          {Allow, Allow},
		"grep '^$' '" + secret + "'":                   {Deny, Allow},
		"cat $'" + root + "/state/vault\\x2edb'":       {Deny, Allow},
		"grep \"$X\" f":                                {Deny, Allow},
		"cat ../x && head -5 memory/../memory/a.md":    {Allow, Allow},
	}
	for name, deny := range map[string]Rule{"absolute": {Tool: "Read", Arg: secret}, "glob": {Tool: "Read", Arg: "**/state/vault.db"}} {
		for cmd, want := range rows {
			with := Permissions{Mode: "plan", Allow: base, Deny: []Rule{deny}}
			if got := with.Decide("Bash", cmd); got != want[0] {
				t.Errorf("%s deny: %q = %v, want %v", name, cmd, got, want[0])
			}
			if got := (Permissions{Mode: "plan", Allow: base}).Decide("Bash", cmd); got != want[1] {
				t.Errorf("no deny: %q = %v, want %v", cmd, got, want[1])
			}
		}
		if got := (Permissions{Mode: "plan", Allow: base, Deny: []Rule{deny}}).DecideAt(root, "Bash", "cat state/vault.db"); got != Deny {
			t.Errorf("%s deny: DecideAt(root, cat state/vault.db) = %v, want Deny", name, got)
		}
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
