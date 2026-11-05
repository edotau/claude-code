package shell

import (
	"os"
	"path/filepath"
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
		if got := gitCommitAllowed(c.args); got != c.want {
			t.Errorf("gitCommitAllowed(%q) = %v, want %v", c.args, got, c.want)
		}
	}
}

// TestCommandGoEnvAndGitGrep pins that only GOOS/GOARCH/CGO_ENABLED may prefix go, and git grep never opens a pager.
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
		{"GOOS=darwin ls", false},
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
		if got := Command(c.cmd, []string{root}, nil); got != c.want {
			t.Errorf("Command(%q) = %v, want %v", c.cmd, got, c.want)
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
		if got := Command(c.cmd, []string{root}, nil); got != c.want {
			t.Errorf("Command(%q) = %v, want %v", c.cmd, got, c.want)
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
		{"cd sub && rm c.go", false},
		{"rm dirlink/record", false}, {"rm SETTINGS.JSON", false}, {"rm VAULT.DB", false}, {"rm X.CREDENTIALS.JSON", false},
		{"rm STATE/x", false}, {"rm " + filepath.Join(root, "STATE", "x"), false}, {"rm viagit/config", false}, {"rm metadata/config", true},
	}
	for _, c := range cases {
		if got := Command(c.cmd, []string{root, wt}, nil); got != c.want {
			t.Errorf("Command(%q) = %v, want %v", c.cmd, got, c.want)
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
	if Command("rm .git/config", []string{root, wt}, nil) {
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
