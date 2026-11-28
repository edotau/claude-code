package rtkwrap

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/shell"
)

// buildFastPathCases synthesizes the FastPath grammar test matrix from rtkRules and guards.json.
func buildFastPathCases() []string {
	var out []string
	for _, h := range []string{"npm-check", "echo", "printf", "cd", "mkdir", "rm", "export", "which", "type", "sed", "true", "tee"} {
		out = append(out, h+" git status", h+" 'go test'")
	}
	heads := map[string]bool{}
	for _, r := range rtkRules {
		heads[r.prefixes[0]] = MayRewrite(r.prefixes[0]) // FastPath answers "" for a word guards.json lacks
	}
	maps.DeleteFunc(heads, func(_ string, may bool) bool { return !may })
	for _, head := range slices.Sorted(maps.Keys(heads)) {
		out = append(out, head, head+" status", head+" run x", head+" -a", head+` "dir with spaces"`)
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
	if got, ok := FastPath("git -c k=v commit -m msg"); got != "" || !ok {
		t.Errorf("FastPath(git -c k=v commit) = %q, %v; want \"\", true", got, ok)
	}
	if got, _ := rewriteCommand(`ls "dir with spaces"`, nil); got != "rtk "+`ls "dir with spaces"` {
		t.Errorf("rewriteCommand(quoted ls) = %q", got)
	}
}

var update = flag.Bool("update", false, "re-record testdata/rtk-rewrite.json from the installed rtk")

// rtkAnswers is the recorded `rtk rewrite` output per command ("" = left as typed), from testdata/rtk-rewrite.json.
const rtkAnswers = "testdata/rtk-rewrite.json"

// harnessRTKConfig is the config the harness renders; the recording and the fast path both read this copy.
const harnessRTKConfig = "[hooks]\nexclude_commands = [\"go vet\", \"go test -v\", \"go test -list\"]\n\n" +
	"[telemetry]\nenabled = false\n"

// rtkHome is a throwaway home holding the harness's rtk config, so neither the fast path nor rtk reads the machine's.
func rtkHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv(paths.RTKBinaryEnv, "") // no stand-in: the fast path answers for the real rtk
	cfg := paths.RTKConfig()
	if cfg == "" {
		t.Skip("rtk keeps no config on this system")
	}
	if err := os.MkdirAll(filepath.Dir(cfg), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte(harnessRTKConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

// recordedRTK reads rtkAnswers.
func recordedRTK(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(rtkAnswers)
	if err != nil {
		t.Fatal(err)
	}
	recorded := map[string]string{}
	if err := json.Unmarshal(raw, &recorded); err != nil {
		t.Fatal(err)
	}
	return recorded
}

// TestRTKFastPathMatchesRTK holds every decided FastPath answer to rtk's own recorded rewrite; -update re-records.
func TestRTKFastPathMatchesRTK(t *testing.T) {
	home := rtkHome(t)
	cases := buildFastPathCases()
	if *update {
		recordRTK(t, home, cases)
	}
	recorded := recordedRTK(t)
	decided := 0
	for _, cmd := range cases {
		got, ok := FastPath(cmd)
		if !ok {
			continue
		}
		decided++
		want, known := recorded[cmd]
		if !known {
			t.Fatalf("no recorded rtk answer for %q: go test -run '^TestRTKFastPathMatchesRTK$' ./internal/platform/rtkwrap/ -update", cmd)
		}
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

// recordRTK asks the installed rtk, with home's config, for every case and writes rtkAnswers.
func recordRTK(t *testing.T, home string, cases []string) {
	t.Helper()
	bin := paths.RTKBinary()
	if bin == "" {
		t.Fatal("-update needs rtk installed")
	}
	recorded := make(map[string]string, len(cases))
	for _, cmd := range cases {
		recorded[cmd] = realRewrite(t, bin, home, cmd)
	}
	raw, err := json.MarshalIndent(recorded, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rtkAnswers, append(raw, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func vetoedByHarness(cmd string) bool {
	f := strings.Fields(cmd)
	return len(f) > 1 && f[0] == "git" && f[1] == "commit"
}

// realRewrite is rtk rewrite's changed output for cmd, "" when it leaves cmd as typed.
func realRewrite(t *testing.T, bin, home, cmd string) string {
	t.Helper()
	c := exec.Command(bin, "rewrite", "--", cmd)
	c.Env = append(os.Environ(), "HOME="+home, paths.RTKQuietEnv+"=1", paths.RTKTelemetryEnv+"=1")
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

// TestRTKFastPathDecisions pins rtk 1.1.0's observed answers (`rtk rewrite`, live config's excludes) for each shape.
func TestRTKFastPathDecisions(t *testing.T) {
	for _, tc := range []struct{ cmd, want string }{
		{"npm-check x", ""}, // no rtk word: FastPath answers before any rule
		{"git commit -m 'go test'", ""},
	} {
		if got, ok := FastPath(tc.cmd); got != tc.want || !ok {
			t.Errorf("FastPath(%q) = %q, %v; want %q, true", tc.cmd, got, ok, tc.want)
		}
	}
	excl := []string{"go vet", "go test -v", "go test -list"}
	for _, tc := range []struct{ cmd, want string }{
		{"echo git status", ""},
		{"rtk git status", "rtk git status"},
		{"git grep x", ""},
		{"go test -v ./x", ""},
		{"cat foo", "rtk read foo"},
		{"head -5 foo 2>&1", "rtk read foo --head-lines 5 2>&1"},
		{"git status | head", "rtk git status | head"},
		{"git log | grep x", "git log | rtk grep x"},
		{"git status && ls", "rtk git status && rtk ls"},
		{"FOO=1 git status", "FOO=1 rtk git status"},
		{"echo $(git status)", ""}, // a substitution, a heredoc or a file redirect defers the whole command
		{"git status; echo \"`date`\"", ""},
		{"git status\ncat <<EOF\nx\nEOF", ""},
		{"git status > out", ""},
		{"git status; echo '$(x) `y` <<z'", "rtk git status; echo '$(x) `y` <<z'"},
		{"nice git status", "nice rtk git status"},
		{"bash -c 'git status && ls'", "bash -c 'rtk git status && rtk ls'"},
		{"mise run x", "rtk mise run x"},
		{"gh pr view --json title", ""},
	} {
		if got, certain := rewriteCommand(tc.cmd, excl); got != tc.want || !certain {
			t.Errorf("rewriteCommand(%q) = %q, %v; want %q, true", tc.cmd, got, certain, tc.want)
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
