package shell

import "testing"

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
