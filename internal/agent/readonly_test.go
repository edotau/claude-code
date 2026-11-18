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
		{"git diff --stat", true},
		{"git log -p -1", true},
		{"git show HEAD", true},
		{"git diff --no-ext-diff --no-textconv", true},
		{"git show --no-textconv --text HEAD", true},
		{"git diff --no-index a b", true},
		{"git -C /tmp log", true},
		{"git -C /abs/repo status --short", true},
		{"git -C /abs/repo status --short | wc -l", true},
		{"git -C /abs/repo log --oneline -5", true},
		{"git -C /abs/repo diff --stat", true},
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
		"git -C d --exec-path=/x status", "git diff --ext-diff", "git log --ext-diff", "git diff --textconv",
		"git show --textconv HEAD", "git diff --ext-d", "git show --textc HEAD", "git -C /abs/repo diff --ext-diff",
		"git -C /abs/repo log --ext-diff", "git -C /abs/repo diff --textconv", "git -C /abs/repo show --textconv", "git --exec-path=/x -C d status", "git --no-pager --exec-path=/x status",
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
		{"rtk find . -name y", true}, {"rtk find . -delete", false}, {"rtk go test ./...", false}, {"rtk make lint", false}, {"rtk proxy rm x", false},
		{"rtk run ls", false}, {"rtk rtk git status", false}, {"rtk git -C /abs/repo stash", false},
		{"rtk git diff --stat", true}, {"rtk git diff --ext-diff", false}, {"rtk git -C /abs/repo show --textconv", false},
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

func TestReadOnlyGitReadForms(t *testing.T) {
	var rows []roCase
	for _, c := range []string{
		"git branch", "git branch -a", "git branch -r", "git branch -v", "git branch -vv", "git branch --list", "git branch -l",
		"git branch --show-current", "git branch --contains HEAD", "git branch --merged", "git branch --no-merged main",
		"git branch --format=%(refname) --sort=-committerdate", "git branch -a --list feat*", "git branch -r --merged main",
		"git remote", "git remote -v", "git remote show origin", "git remote get-url origin",
		"git worktree list", "git worktree list --porcelain",
		"git stash list", "git stash show", "git stash show -p stash@{1}",
		"git clean -n", "git clean --dry-run", "git clean -nd", "git clean -ndx", "git clean -fn", "git clean -f -d --dry-run",
		"git reflog", "git reflog -5", "git reflog show HEAD", "git reflog show --date=iso",
	} {
		rows = append(rows, roCase{c, true})
	}
	for _, c := range []string{
		"git branch new", "git branch -d x", "git branch -D x", "git branch -m a b", "git branch -M a b", "git branch -c a b",
		"git branch -C a b", "git branch -f x", "git branch --set-upstream-to=origin/x", "git branch -u origin/x",
		"git branch --unset-upstream", "git branch --edit-description", "git branch -v new", "git branch -a new",
		"git branch --list -D x", "git branch --show-current new", "git branch --delete x", "git branch --del x",
		"git remote add o u", "git remote remove o", "git remote rm o", "git remote rename a b", "git remote set-url o u",
		"git remote set-head o x", "git remote prune o", "git remote update", "git remote show ext::sh", "git remote get-url",
		"git remote -v add o u", "git remote show origin extra",
		"git worktree add /tmp/w", "git worktree remove w", "git worktree prune", "git worktree move a b", "git worktree lock w",
		"git worktree", "git worktree list --output=x",
		"git stash", "git stash push", "git stash pop", "git stash drop", "git stash apply", "git stash clear",
		"git stash branch x", "git stash create", "git stash store x", "git stash show --output=x",
		"git clean", "git clean -f", "git clean -fd", "git clean -fdx", "git clean -d", "git clean -n -i", "git clean -e x -n",
		"git clean --force", "git clean --no-dry-run", "git clean -i", "git clean -n -- /etc",
		"git reflog expire --all", "git reflog delete HEAD@{1}", "git reflog exists x", "git reflog HEAD",
		"git reflog show --output=x",
	} {
		rows = append(rows, roCase{c, false})
	}
	checkReadOnly(t, rows)
}

func TestReadOnlyFindEchoPrintf(t *testing.T) {
	checkReadOnly(t, []roCase{
		{`find . -name "*.go"`, true}, {"rtk find . -name '*.go'", true}, {"find . -type f -print0", true},
		{"find . -maxdepth 2 ( -name a -o -name b ) ! -path x -ls", true}, {"find . -newer a -print", true},
		{"find . -exec rm {} ;", false}, {"find . -execdir rm {} ;", false}, {"find . -ok rm {} ;", false},
		{"find . -okdir rm {} ;", false}, {"find . -delete", false}, {"find . -fprint out", false},
		{"find . -fprint0 out", false}, {"find . -fprintf out %p", false}, {"find . -fls out", false},
		{`find . -name x "-exec" rm`, false}, {`find . -name x \-delete`, false}, {"find . -name x | xargs rm", false},
		{"find . > out", false},
		{`echo "git reset --hard"`, true}, {`printf "%s" "rm -rf /"`, true}, {`echo hi; printf '%s\n' a`, true},
		{"echo hi > f", false}, {"echo $(rm x)", false}, {"echo `rm x`", false}, {"printf '%s\\n' a | sh", false},
		{"printf x >> f", false}, {"echo hi | tee f", false},
	})
}

func TestReadOnlyClusterCLIs(t *testing.T) {
	var rows []roCase
	for _, c := range []string{
		"kubectl get pods", "kubectl logs deployment/app", "kubectl describe pod x", "kubectl top pods", "kubectl version",
		"kubectl api-resources", "kubectl explain pod", "kubectl config get-contexts", "kubectl config current-context",
		"kubectl -n ns get pods", "kubectl --namespace ns get pods", "kubectl --namespace=ns get pods", "kubectl --context x get pods -o wide",
		"oc get pods", "oc -n ns logs x", "oc config current-context", "kubectl get pods | wc -l",
		"docker ps", "docker images", "docker inspect x", "docker logs x", "docker version", "docker info", "docker --context x ps",
		"docker --context=x ps -a",
		"gh pr view 42", "gh pr list", "gh pr status", "gh pr diff 42", "gh pr checks 42", "gh issue view 1", "gh issue list",
		"gh issue status", "gh repo view", "gh run view 1", "gh run list", "gh release view v1", "gh release list", "gh auth status",
		"aws --version", "aws help", "aws sts get-caller-identity", "gcloud --version", "gcloud version", "gcloud help",
		"gcloud config list",
	} {
		rows = append(rows, roCase{c, true})
	}
	for _, c := range []string{
		"kubectl", "kubectl config view", "kubectl config view --raw", "kubectl config use-context x", "kubectl config set x y",
		"kubectl apply -f x", "kubectl delete pod x", "kubectl exec x -- sh", "kubectl port-forward x 1", "kubectl cp a b",
		"kubectl edit x", "kubectl patch x", "kubectl run x", "kubectl auth can-i x", "kubectl create x",
		"kubectl get secret x -o yaml", "kubectl get secrets", "kubectl get cm,secret", "kubectl get secret/x", "kubectl describe secret x",
		"kubectl get pods --raw /api", "kubectl get pods -w", "kubectl logs x -f", "kubectl logs x --follow", "kubectl get pods --token x",
		"kubectl get pods --kubeconfig /x", "kubectl get pods --server https://x", "kubectl --kubeconfig /x get pods",
		"kubectl --server x get pods", "kubectl -n get pods", "kubectl -n --context get pods", "kubectl --as x get pods",
		"oc delete x", "oc whoami -t", "oc login", "oc config view",
		"docker run x", "docker rm x", "docker exec x sh", "docker build .", "docker pull x", "docker stop x", "docker logs -f x",
		"docker -H tcp://x ps", "docker --host x ps", "docker context use x", "docker --context ps",
		"gh api /user", "gh auth token", "gh auth login", "gh auth status --show-token", "gh auth status -t", "gh pr create",
		"gh pr merge 1", "gh pr checkout 1", "gh pr view 1 --web", "gh pr checks 1 --watch", "gh issue create", "gh repo clone x",
		"gh repo view -w", "gh run rerun 1", "gh release create v1", "gh release download v1", "gh -R a/b pr view 1", "gh",
		"aws", "aws s3 ls", "aws ec2 describe-instances", "aws sts get-session-token", "aws ecr get-login-password",
		"aws sts assume-role x", "aws configure list", "aws --profile x sts get-caller-identity",
		"gcloud auth print-access-token", "gcloud auth print-identity-token", "gcloud config set x y", "gcloud compute list",
		"gcloud auth list", "gcloud",
		"databricks auth token", "databricks clusters list", "databricks jobs delete 1",
		"env", "env FOO=1 ls", "printenv", "az account get-access-token",
	} {
		rows = append(rows, roCase{c, false})
	}
	checkReadOnly(t, rows)
}
