package shell

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestGitCommitAllowedRefusesReuse pins that -c/-C (reuse another commit's message) are refused, clustered or not.
func TestGitCommitAllowedRefusesReuse(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{[]string{"-c", "HEAD", "-m", "x"}, false},
		{[]string{"-C", "HEAD"}, false},
		{[]string{"-cHEAD"}, false},
		{[]string{"-m", "x"}, true},
	}
	for _, c := range cases {
		if got := gitCommitAllowed(c.args, "."); got != c.want {
			t.Errorf("gitCommitAllowed(%q) = %v, want %v", c.args, got, c.want)
		}
	}
}

// TestCommandGoEnvAndGitGrep pins that only the Go env names may prefix an allowed command, and git grep never opens a pager.
func TestCommandGoEnvAndGitGrep(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		cmd  string
		want bool
	}{
		{"GOOS=darwin go vet ./internal/x/...", true},
		{"GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./...", true},
		{"git grep -n foo", true},
		{"LD_PRELOAD=x go vet ./...", false},
		{"GOFLAGS=-toolexec=x go vet ./...", false},
		{"GOOS=darwin LD_PRELOAD=x go vet ./...", false},
		{"GOOS=darwin rm -rf x", false},
		{"GOOS=darwin ls", true},
		{"GOOS=darwin env go vet ./...", false},
		{"GOOS=darwin go vet -vettool=x ./...", false},
		{"GOOS=darwin go run ./...", false},
		{"GOOS=darwin", false},
		{"GOOS=dar-win go vet ./...", false},
		{"git grep -O foo", false},
		{"git grep -nO foo", false},
		{"git grep --open-files-in-pager foo", false},
		{"git grep --open-files-in-pager=less foo", false},
		{"git grep --open foo", false},
	}
	for _, c := range cases {
		if got := CommandIn(c.cmd, root, []string{root}, nil); got != c.want {
			t.Errorf("CommandIn(%q) = %v, want %v", c.cmd, got, c.want)
		}
	}
}

// TestCommandDashFormsAndPathValues pins double-dash banned flags, attached path values and git grep flag parsing.
func TestCommandDashFormsAndPathValues(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		cmd  string
		want bool
	}{
		{"go vet --vettool=/usr/bin/true ./...", false},
		{"GOOS=darwin go vet --vettool=/usr/bin/true ./...", false},
		{"go build --toolexec=/usr/bin/true ./...", false},
		{"go test --exec=/usr/bin/true ./...", false},
		{"go build -o=/tmp/x ./cmd", false},
		{"go build -o/tmp/x ./cmd", false},
		{"go build --o=/tmp/x ./cmd", false},
		{"go build -modfile=/tmp/go.mod ./...", false},
		{"go test -coverprofile=/tmp/c.out ./...", false},
		{"go build -o=../x ./cmd", false},
		{"go build -o=bin/x ./cmd", true},
		{"go test ./internal/shell -outputdir=/outside -cpuprofile=proof.out", false},
		{"go test ./internal/shell -test.cpuprofile=/outside/p.out", false},
		{"go test ./internal/shell -blockprofile=/outside/p.out", false},
		{"go test ./internal/shell --blockprofile='~/p.out'", false},
		{"go test ./internal/shell -cpuprofile=proof.out", true},
		{"go build -oout/x ./cmd", true},
		{"git grep -f /tmp/p foo", false},
		{"git grep -f/tmp/p foo", false},
		{"git grep --file=/tmp/p foo", false},
		{"git grep -nf ../p foo", false},
		{"git grep -f pats foo", true},
		{"git log --no-ext-diff --no-textconv -SFOO", true},
		{"git diff --no-ext-diff --no-textconv -Oorderfile", true},
		{"git grep -eFOO", true},
		{"git grep -e foo -nO bar", false},
	}
	for _, c := range cases {
		if got := CommandIn(c.cmd, root, []string{root}, nil); got != c.want {
			t.Errorf("CommandIn(%q) = %v, want %v", c.cmd, got, c.want)
		}
	}
}

// TestCommandRm pins that plain rm admits only existing regular files inside the roots, clear of .git, state and settings.
func TestCommandRm(t *testing.T) {
	home, outside := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, ".claude")
	for _, f := range []string{"a.go", "b.go", "sub/c.go", ".git/config", "state/vault.db", "state/other.txt", "-f", "settings.json", "settings.local.json"} {
		p := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	wt := filepath.Join(home, "worktrees")
	for _, f := range []string{"metadata/config", "SETTINGS.JSON", "VAULT.DB", "X.CREDENTIALS.JSON", "STATE/x"} {
		touch(t, filepath.Join(root, f))
	}
	touch(t, filepath.Join(outside, "record"))
	touch(t, filepath.Join(wt, "dirlink", "record")) // a decoy under the second root
	mustSymlink(t, outside, filepath.Join(root, "dirlink"))
	mustSymlink(t, ".git", filepath.Join(root, "viagit"))
	for link, target := range map[string]string{"link-out": outside, "link-in": filepath.Join(root, "a.go")} {
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		cmd  string
		want bool
	}{
		{"rm a.go", true}, {"rm a.go b.go sub/c.go", true}, {"rm " + filepath.Join(root, "a.go"), true},
		{"rm", false}, {"rm -f a.go", false}, {"rm -f", false}, {"rm state/other.txt", false}, {"rm -r sub", false}, {"rm -- -rf", false}, {"rm --force a.go", false},
		{"rm '*.go'", false}, {"rm 'a.?o'", false}, {"rm '~/x'", false}, {"rm ../x.go", false}, {"rm sub/../../x", false},
		{"rm /etc/passwd", false}, {"rm .git/config", false}, {"rm state/vault.db", false}, {"rm settings.json", false},
		{"rm settings.local.json", false}, {"rm link-out", false}, {"rm link-in", false}, {"rm link-out/x", false},
		{"rm sub", false}, {"rm missing.go", false}, {"rm a.go sub", false}, {"rm a.go; rm b.go", true},
		{"cd sub && rm c.go", true}, {"cd sub | rm c.go", false}, {"cd sub; rm c.go", false}, {"cd sub || rm c.go", false},
		{"rm dirlink/record", false}, {"rm SETTINGS.JSON", false}, {"rm VAULT.DB", false}, {"rm X.CREDENTIALS.JSON", false},
		{"rm STATE/x", false}, {"rm " + filepath.Join(root, "STATE", "x"), false}, {"rm viagit/config", false}, {"rm metadata/config", true},
	}
	for _, c := range cases {
		if got := CommandIn(c.cmd, root, []string{root, wt}, nil); got != c.want {
			t.Errorf("CommandIn(%q) = %v, want %v", c.cmd, got, c.want)
		}
	}
	for _, cwd := range []string{"", "sub", outside} {
		for _, cmd := range []string{"rm a.go", "ls"} {
			if CommandIn(cmd, cwd, []string{root, wt}, nil) {
				t.Errorf("CommandIn allowed %q from cwd %q outside the roots", cmd, cwd)
			}
		}
	}
	if !CommandIn("rm c.go", filepath.Join(root, "sub"), []string{root, wt}, nil) {
		t.Error("rm c.go from cwd root/sub was refused")
	}
	// .git itself is a symlink to an in-root dir: the lexical .git component must still refuse.
	if err := os.RemoveAll(filepath.Join(root, ".git")); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, "metadata", filepath.Join(root, ".git"))
	if CommandIn("rm .git/config", root, []string{root, wt}, nil) {
		t.Error("rm .git/config through a symlinked .git was allowed")
	}
}

func touch(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func TestReadOnlyGitLeadingNoPager(t *testing.T) {
	for cmd, want := range map[string]bool{
		"git --no-pager status":                           true,
		"git --no-pager status --short --branch":          true,
		"git --no-pager diff --no-ext-diff --no-textconv": true,
		"git --no-pager push":                             false,
		"git --no-pager --no-pager status":                false,
		"git --no-pager=x status":                         false,
		"git -c core.pager=x status":                      false,
		"git --config-env=a=b status":                     false,
		"git --paginate status":                           false,
		"git --no-pager":                                  false,
	} {
		if got := ReadOnlyGit(cmd); got != want {
			t.Errorf("ReadOnlyGit(%q) = %v, want %v", cmd, got, want)
		}
	}
	if !scopedGit([]string{"git", "-C", ".", "--no-pager", "status"}, t.TempDir()) {
		t.Error("scopedGit -C . --no-pager status refused")
	}
}

// TestGitCommitAllowedPathspecInsideWorkDir pins that commit pathspecs obey git add's containment.
func TestGitCommitAllowedPathspecInsideWorkDir(t *testing.T) {
	root := t.TempDir()
	touch(t, filepath.Join(root, "agents", "a.md"))
	cases := []struct {
		args []string
		want bool
	}{
		{[]string{"-m", "x", "/etc/passwd"}, false},
		{[]string{"-m", "x", "../other/file"}, false},
		{[]string{"-m", "x", "agents/a.md"}, true},
		{[]string{"-m", "x", filepath.Join(root, "agents", "a.md")}, true},
		{[]string{"-m", "x", "agents"}, false},
		{[]string{"-m", "x"}, true},
	}
	for _, c := range cases {
		if got := gitCommitAllowed(c.args, root); got != c.want {
			t.Errorf("gitCommitAllowed(%q) = %v, want %v", c.args, got, c.want)
		}
	}
}

// TestCommandInGitDirOperand pins that git add/commit refuse a directory operand even when a second root lacks that dir.
func TestCommandInGitDirOperand(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root, wt := filepath.Join(home, ".claude"), filepath.Join(home, "worktrees")
	touch(t, filepath.Join(root, "agents", "a.md"))
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		cmd  string
		want bool
	}{
		{"git add agents", false}, {"git commit -m x agents", false}, {"git add agents/", false}, {"git add .", false},
		{"git add agents/a.md", true}, {"git commit -m x agents/a.md", true},
	}
	for _, c := range cases {
		if got := CommandIn(c.cmd, root, []string{root, wt}, nil); got != c.want {
			t.Errorf("CommandIn(%q) = %v, want %v", c.cmd, got, c.want)
		}
	}
}

func TestArgsInsideAbsoluteOperandOtherRoot(t *testing.T) {
	root, wt := additionRoots(t)
	operand := filepath.Join(wt, "x", "f")
	if !argsInside([]string{operand}, root, wt) {
		t.Errorf("argsInside refused absolute operand %q in another allowed root from cwd %q", operand, root)
	}
	outside := filepath.Join(t.TempDir(), "f")
	touch(t, outside)
	if argsInside([]string{outside}, root, wt) {
		t.Errorf("argsInside allowed absolute operand %q outside the allowed root %q", outside, wt)
	}
}

// TestAbsolutizeOperandsGitDirAndSlash pins that git -C dir gets cwd and a git slash-suffixed path stays as written.
func TestAbsolutizeOperandsGitDirAndSlash(t *testing.T) {
	cases := []struct{ in, want []string }{
		{[]string{"git", "-C", "sub", "status"}, []string{"git", "-C", "/w/sub", "status"}},
		{[]string{"git", "add", "agents/"}, []string{"git", "add", "agents/"}},
		{[]string{"rm", "a/"}, []string{"rm", "/w/a"}},
	}
	for _, c := range cases {
		if got := absolutizeOperands(c.in, "/w"); !slices.Equal(got, c.want) {
			t.Errorf("absolutizeOperands(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestCanonicalSelfInstalledSymlink pins that .local/bin/claude-code is rewritten only when it resolves to the harness binary.
func TestCanonicalSelfInstalledSymlink(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	self := filepath.Join(home, ".claude", "bin", "claude-code")
	other := filepath.Join(home, "other")
	for _, p := range []string{self, other} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(home, ".local", "bin", "claude-code")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := link + " doctor"
	if got := CanonicalSelf(cmd); got != cmd {
		t.Errorf("missing link rewritten: %q", got)
	}
	if err := os.Symlink(other, link); err != nil {
		t.Fatal(err)
	}
	if got := CanonicalSelf(cmd); got != cmd {
		t.Errorf("lookalike link rewritten: %q", got)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(self, link); err != nil {
		t.Fatal(err)
	}
	for _, in := range []string{link + " doctor", "~/.local/bin/claude-code doctor", "$HOME/.local/bin/claude-code doctor"} {
		if got := CanonicalSelf(in); got != "claude-code doctor" {
			t.Errorf("CanonicalSelf(%q) = %q", in, got)
		}
	}
}

// additionRoots builds a hermetic $HOME/.claude root (with sub/f) and a ~/worktrees/x root for the allowlist additions.
func additionRoots(t *testing.T) (root, wt string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	root, wt = filepath.Join(home, ".claude"), filepath.Join(home, "worktrees")
	touch(t, filepath.Join(root, "sub", "f"))
	touch(t, filepath.Join(wt, "x", "f"))
	return root, wt
}

// TestCommandAllowlistAdditions pins the intended forms of mktemp, cd chains, mkdir, text tools, Go env and </dev/null.
func TestCommandAllowlistAdditions(t *testing.T) {
	root, wt := additionRoots(t)
	for _, cmd := range []string{
		`mktemp "${TMPDIR:-/tmp}/copilot-task.XXXXXX"`, "mktemp $TMPDIR/x.XXXXXX", "mktemp", "mktemp -d",
		"cd sub && go test ./...", "cd " + filepath.Join(wt, "x") + " && ls f", "cd sub && cd .. && ls",
		"mkdir -p sub/new/dir", "mkdir -p " + filepath.Join(root, "abs"),
		`printf '%s' x | cut -d: -f1 | tr a-z A-Z | sort | uniq -c`, "basename a/b.go && dirname a/b.go && pwd", "uniq sub/f",
		"GOCACHE=/tmp/gocache GOTMPDIR=/tmp GOFLAGS=-count=1 go test ./...", `GOFLAGS="-mod=mod -count=1" make test`,
		"go test ./... < /dev/null", "go vet ./...</dev/null 2>&1",
	} {
		if !CommandIn(cmd, root, []string{root, wt}, nil) {
			t.Errorf("CommandIn(%q) refused, want allowed", cmd)
		}
	}
}

// TestCommandAllowlistAdditionAbuses pins that each addition's escape shape stays blocked.
func TestCommandAllowlistAdditionAbuses(t *testing.T) {
	root, wt := additionRoots(t)
	for _, cmd := range []string{
		"cd /etc && ls", "cd .. && ls", "cd", "cd -", "cd sub extra", "ls | cd sub && ls f", "cd sub && ls; ls f",
		"GOOS=darwin cd /etc", "cd $TMPDIR && ls",
		"mkdir /tmp/x", "mkdir -p ../x",
		"GOFLAGS=-toolexec=x go test ./...", `GOFLAGS="-count=1 --exec=x" make test`, "GOFLAGS=-modfile=/tmp/go.mod go test ./...",
		"LD_PRELOAD=x go test ./...", "PATH=/tmp go test ./...", "GOOS=darwin",
		`GOFLAGS="'-toolexec=/tmp/evil'" go test ./...`, `GOFLAGS='"-exec=x"' go test ./...`, `GOFLAGS="-count=1 '-vet=off'" make test`,
		"printf x > f", "printf -v PATH /tmp && go test ./...", "printf -vPATH x", "sed -n 1p f", "cut -f1 /etc/passwd",
		"printf %n PATH && go test ./...", "printf '%5n' IFS", "printf '%ln' X", "printf '%*n' 3 X", "printf -- 'a%jn' X",
		"uniq sub/f out", "uniq -c sub/f out", "uniq -- -a -b",
		"mktemp x && cat > /etc/y", "ls $TMPDIR", "cat ${TMPDIR:-/tmp}/x", "mktemp ${HOME}/x", "mktemp $TMPDIRX",
		"mktemp '$TMPDIR/x'", "f=$(mktemp)", "GOOS=darwin mktemp $TMPDIR/x",
		"ls < /etc/passwd", "ls << /dev/null", "ls </dev/nullx",
	} {
		if CommandIn(cmd, root, []string{root, wt}, nil) {
			t.Errorf("CommandIn(%q) allowed, want blocked", cmd)
		}
	}
}
