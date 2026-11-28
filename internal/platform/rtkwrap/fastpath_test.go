package rtkwrap

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/shell"
)

// buildFastPathCases synthesizes the FastPath grammar test matrix from simpleRules and guards.json.
func buildFastPathCases() []string {
	var out []string
	for _, h := range []string{"npm-check", "echo", "printf", "cd", "mkdir", "rm", "export", "which", "type", "sed", "true", "tee"} {
		out = append(out, h+" git status", h+" 'go test'")
	}
	for _, head := range slices.Sorted(maps.Keys(simpleRules)) {
		r := simpleRules[head]
		switch {
		case len(r.subs) > 0:
			out = append(out, head, head+" unknown-sub x")
			for _, sub := range r.subs {
				out = append(out, head+" "+sub, head+" "+sub+" x")
			}
		case r.minArgs > 0:
			out = append(out, head, head+" x", head+" -n x")
		default:
			out = append(out, head, head+" -a", head+" 'a b'", head+` "dir with spaces"`)
		}
	}
	for _, flag := range shell.GuardWords("rtk_go_test_raw_flags") {
		out = append(out, "go test -"+flag+" ./x")
	}
	for _, w := range []string{"env", "sudo", "nice", "time", "noglob", "xargs"} {
		out = append(out, w+" git status", w+" ls")
	}
	for _, tmpl := range []string{
		"FOO=1 %s", "%s | wc -l", "%s && ls", "echo hi; %s", "true || %s", "%s\nls",
		"%s > out", "%s 2>&1", "%s # git", "echo $(%s)", "echo `%s`", "bash -c '%s'",
		"'%s'", "./%s", "/bin/%s", "(%s)", "{ %s; }", "if %s; then :; fi", "! %s", "%s &",
	} {
		out = append(out, fmt.Sprintf(tmpl, "ls"))
	}
	return append(out,
		"git grep x", "go vet ./...", "go  test -v", "cd x && go test -v",
		"rtk git status", "rtk bypass git status", "rtk proxy git status", "rtk git status && echo done",
		"cat foo", "head -5 foo", "tail -f x", "ls  -la", "git -C foo status", "git -c k=v status",
		"mise run x", "shellcheck x", "biome x", "cat <<EOF\ngit status\nEOF",
	)
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
	cases := buildFastPathCases()
	if len(cases) < 40 {
		t.Fatalf("only %d cases", len(cases))
	}
	decided := 0
	for _, cmd := range cases {
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
		t.Errorf("FastPath decided only %d of %d cases", decided, len(cases))
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
