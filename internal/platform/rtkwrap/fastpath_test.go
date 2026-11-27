package rtkwrap

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/platform/paths"
)

// fastPathCases span skips, simple rewrites, exclusions, prefixed and quoted mentions, wrappers, pipes and chains.
var fastPathCases = []string{
	// no rtk word, or one only in argument position
	"npm-check", "echo hello", "echo git status", "echo 'git status'", `echo "go test"`, "printf '%s' git",
	"cd ~/go", "mkdir -p go/test", "rm -rf go", "export X=git", "which git", "type git", "sed -n 1p go.mod",
	"cd x && echo go", "echo a > git", "pwd; echo ls", "true", "tee go",
	// simple rewrites
	"git status", "git status -s", "git log --oneline -5", "git diff HEAD~1", "git show --stat", "git branch -a",
	"git add -A", "git push origin HEAD", "git pull --rebase", "git fetch --all", "git stash list",
	"go test ./...", "go test -run 'X|Y' ./...", "go test -json ./...", "go build -o x .", "go test -v2",
	"go test '-v'", "ls", "ls -la", "ls *.go", "ls 'a b'", "find . -name '*.md'", "grep -rn TODO .",
	"grep -E 'a|b' f", "rg -n foo internal", "wc -l x", "du -sh", "df -h", "ps aux", "stat f", "tree -L 2",
	"gh pr list", "gh api x", "docker ps -a", "docker logs -f x", "kubectl get pods", "kubectl describe pod x",
	"curl -s http://x", "diff -u a b", "make", "make test V=1", "jq -r .a f", "golangci-lint run ./...",
	// heads rtk declines or rewrites differently: never decided as rewrites
	"find", "grep", "rg", "curl", "diff", "git", "go", "kubectl", "gh", "docker compose up", "golangci-lint version",
	"gh auth status", "docker inspect x", "go run .", "go mod tidy", "git rebase -i", "cat foo", "head -5 foo",
	"tail -f x", "ls  -la", "git -C foo status", "mise run x", "shellcheck x", "biome x",
	// excluded
	"git grep x", "go vet ./...", "go test -v ./x", "go test -list x", "go  test -v", "cd x && go test -v",
	// already prefixed
	"rtk git status", "rtk bypass git status", "rtk proxy git status", "rtk git status && echo done",
	// wrappers, quoting, comments, pipes, redirects, substitutions, chains
	"FOO=1 git status", "time ls", "env ls", "nice git status", "noglob git status", "sudo ls", "xargs grep x",
	"bash -c 'ls'", "'ls'", "./ls", "/bin/ls", "(ls)", "{ ls; }", "if ls; then :; fi", "! ls", "ls &",
	"ls > out", "ls 2>&1", "ls | wc -l", "git status | head", "ls # git", "echo $(git status)", "echo `ls`",
	"cat <<EOF\ngit status\nEOF", "git status && ls", "echo hi; ls", "true || ls", "git status\nls",
	"go test -v; ls", "sleep 1; ls", "echo git status | tee x",
	// quoted arguments stay single words; git commit past global options is never wrapped
	`ls "dir with spaces"`, "git -c k=v status",
}

func TestFastPathQuotedAndCommit(t *testing.T) {
	t.Setenv(paths.RTKBinaryEnv, "")
	if got := VetoRule("git -c commit.gpgsign=false commit -m msg", ""); got != "commit" {
		t.Errorf("VetoRule(git -c ... commit) = %q, want commit", got)
	}
	if !segmentStays([]string{"git", "-c", "k=v", "commit", "-m", "msg"}) {
		t.Error("segmentStays(git -c k=v commit) = false, want true")
	}
	if got, ok := simpleRewrite(`ls "dir with spaces"`); ok && got != "rtk "+`ls "dir with spaces"` {
		t.Errorf("simpleRewrite(quoted ls) = %q", got)
	}
}

// TestRTKFastPathMatchesRTK holds every decided FastPath answer to the installed rtk's own rewrite.
func TestRTKFastPathMatchesRTK(t *testing.T) {
	bin := paths.RTKBinary()
	if bin == "" {
		t.Skip("rtk is not installed")
	}
	t.Setenv(paths.RTKBinaryEnv, "") // bin is the rtk the fast path stands in for
	if len(fastPathCases) < 40 {
		t.Fatalf("only %d cases", len(fastPathCases))
	}
	decided := 0
	for _, cmd := range fastPathCases {
		got, ok := FastPath(cmd)
		if !ok {
			continue
		}
		decided++
		want := realRewrite(t, bin, cmd)
		if vetoedByHarness(cmd) {
			want = "" // the harness never wraps a commit, whatever rtk says
		}
		if got != want {
			t.Errorf("FastPath(%q) = %q, rtk rewrite = %q", cmd, got, want)
		}
	}
	if decided < 20 {
		t.Errorf("FastPath decided only %d of %d cases", decided, len(fastPathCases))
	}
}

func vetoedByHarness(cmd string) bool {
	f := strings.Fields(cmd)
	return len(f) > 1 && f[0] == "git" && f[1] == "commit"
}

// realRewrite is rtk rewrite's changed output for cmd, "" when it leaves cmd as typed.
func realRewrite(t *testing.T, bin, cmd string) string {
	t.Helper()
	c := exec.Command(bin, "rewrite", "--", cmd)
	c.Env = append(os.Environ(), paths.RTKQuietEnv+"=1", paths.RTKTelemetryEnv+"=1")
	out, err := c.Output()
	var ee *exec.ExitError
	if err != nil && (!errors.As(err, &ee) || ee.ExitCode() != 3) {
		return ""
	}
	if got := strings.TrimSpace(string(out)); got != cmd {
		return got
	}
	return ""
}

func TestRTKFastPathDecisions(t *testing.T) {
	for _, tc := range []struct {
		cmd, want string
		decided   bool
	}{
		{"npm-check x", "", true},
		{"echo git status", "", true},
		{"git commit -m 'go test'", "", true},
		{"rtk git status", "", true},
		{"git grep x", "", true},
		{"cat foo", "", false},
		{"git status | head", "", false},
		{"git status && ls", "", false},
		{"FOO=1 git status", "", false},
		{"echo $(git status)", "", false},
		{"nice git status", "", false},
		{"mise run x", "", false},
	} {
		got, ok := FastPath(tc.cmd)
		if got != tc.want || ok != tc.decided {
			t.Errorf("FastPath(%q) = %q, %v; want %q, %v", tc.cmd, got, ok, tc.want, tc.decided)
		}
	}
}

func TestRTKFastPathSimpleRewriteNeedsKnownConfig(t *testing.T) {
	ex, ok := parseExcludes([]byte("# Rendered by the claude-code harness.\n\n[hooks]\n" +
		`exclude_commands = ["go vet", "go test -v", "go test -list"]` + "\n\n[telemetry]\nenabled = false\n"))
	if !ok || len(ex) != 3 || strings.Join(ex[1], " ") != "go test -v" {
		t.Fatalf("parseExcludes = %q, %v", ex, ok)
	}
	for _, body := range []string{"[hooks]\nexclude_commands = [\"go.*\"]\n", "[hooks]\nexclude_commands = [\"git\"]\n",
		"[hooks]\nother = 1\n", "[display]\ncolor = true\n", "[hooks]\nexclude_commands = [\n"} {
		if _, ok := parseExcludes([]byte(body)); ok {
			t.Errorf("parseExcludes(%q) accepted", body)
		}
	}
	t.Setenv("RTK_DISABLED", "1")
	if got, ok := FastPath("git status"); ok {
		t.Errorf("FastPath under RTK_DISABLED = %q, decided", got)
	}
}
