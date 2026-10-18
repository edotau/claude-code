package dispatch

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// shRoundTrip runs the quoted string through `sh -c 'printf %s <quoted>'` and returns stdout.
// This is the proof required by the task: execute the generated shell text for real and confirm
// it reproduces the original bytes with no expansion, no execution, no word-splitting.
func shRoundTrip(t *testing.T, quoted string) string {
	t.Helper()
	cmd := exec.Command("sh", "-c", "printf %s "+quoted)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("sh -c failed on quoted=%s: %v", quoted, err)
	}
	return string(out)
}

func TestQuoteSH_RoundTrip(t *testing.T) {
	cases := []string{
		"plain text",
		"it's a test",      // single quote
		`say "hi" to them`, // double quotes
		"$HOME",            // var expansion attempt
		"`whoami`",         // backtick command substitution attempt
		"$(rm -rf ~)",      // command substitution attempt
		"a; rm -rf /",      // command separator
		"a && echo pwned",  // logical and
		"a | cat",          // pipe
		"line1\nline2",     // embedded newline
		"héllo wörld 日本語",  // unicode
		"",                 // empty string
		"'''",              // only single quotes
		"a'b'c",            // quote-delimited segments
	}
	for _, in := range cases {
		t.Run(strconv.Quote(in), func(t *testing.T) {
			quoted := QuoteSH(in)
			got := shRoundTrip(t, quoted)
			if got != in {
				t.Fatalf("QuoteSH(%q) = %s; sh -c printf round-trip = %q, want %q", in, quoted, got, in)
			}
		})
	}
}

func TestQuoteSH_NoInjectionViaEcho(t *testing.T) {
	// Build a full `sh -c` command embedding a hostile payload the way BuildRemoteCommand does,
	// and prove the payload never executes: if it did, the marker file would be created.
	dir := t.TempDir()
	marker := dir + "/pwned"
	payload := "$(touch " + marker + ")"
	cmd := exec.Command("sh", "-c", "echo "+QuoteSH(payload)+" >/dev/null")
	if err := cmd.Run(); err != nil {
		t.Fatalf("sh -c failed: %v", err)
	}
	if _, err := exec.Command("test", "-e", marker).CombinedOutput(); err == nil {
		t.Fatalf("injection succeeded: marker file was created")
	}
}

func TestValidAgent(t *testing.T) {
	for _, a := range Agents {
		if !ValidAgent(a) {
			t.Errorf("ValidAgent(%q) = false, want true", a)
		}
	}
	for _, bad := range []string{"", "bash", "Claude", "claude ", " claude", "claude;rm"} {
		if ValidAgent(bad) {
			t.Errorf("ValidAgent(%q) = true, want false", bad)
		}
	}
}

func TestValidName(t *testing.T) {
	ok := []string{"a", "cc-claude-abcd", "A.B_C-9", strings.Repeat("x", 64)}
	for _, n := range ok {
		if !ValidName(n) {
			t.Errorf("ValidName(%q) = false, want true", n)
		}
	}
	bad := []string{"", "a b", "../x", strings.Repeat("x", 65), "a/b", "a;b", "a$b", "a\nb"}
	for _, n := range bad {
		if ValidName(n) {
			t.Errorf("ValidName(%q) = true, want false", n)
		}
	}
}

func TestBuildRemoteCommand_RejectsBadName(t *testing.T) {
	for _, bad := range []string{"a b", "../x", strings.Repeat("x", 65), " "} {
		if _, _, err := BuildRemoteCommand(Opts{Agent: "claude", Name: bad, Task: "hi"}); err == nil {
			t.Errorf("BuildRemoteCommand with --name %q: want error, got nil", bad)
		}
	}
}

func TestBuildRemoteCommand_RejectsUnknownAgent(t *testing.T) {
	for _, bad := range []string{"", "bash", "Claude", "claude;rm"} {
		if _, _, err := BuildRemoteCommand(Opts{Agent: bad, Task: "hi"}); err == nil {
			t.Errorf("BuildRemoteCommand with --agent %q: want error, got nil", bad)
		}
	}
}

func TestBuildRemoteCommand_SyntaxAndShape(t *testing.T) {
	for _, agent := range Agents {
		t.Run(agent, func(t *testing.T) {
			cmd, name, err := BuildRemoteCommand(Opts{
				Agent: agent,
				Dir:   `/tmp/a dir with "quotes" and spaces`,
				Name:  "cc-" + agent + "-test",
				Task:  `do something; with $(nasty) 'quotes' and "stuff" & | ; \n chars`,
			})
			if err != nil {
				t.Fatalf("BuildRemoteCommand: %v", err)
			}
			if name != "cc-"+agent+"-test" {
				t.Errorf("returned name = %q, want %q", name, "cc-"+agent+"-test")
			}
			// sh -n: syntax check only, nothing executes.
			check := exec.Command("sh", "-n", "-c", cmd)
			if out, err := check.CombinedOutput(); err != nil {
				t.Fatalf("sh -n rejected generated command: %v\n--- command ---\n%s\n--- output ---\n%s", err, cmd, out)
			}
			wantPrefix := "tmux new-session -d -s " + QuoteSH(name) + " -c " + QuoteSH(`/tmp/a dir with "quotes" and spaces`)
			if !strings.Contains(cmd, wantPrefix) {
				t.Errorf("command missing expected tmux invocation.\nwant substring: %s\ngot: %s", wantPrefix, cmd)
			}
		})
	}
}

func TestBuildRemoteCommand_DefaultDirAndName(t *testing.T) {
	cmd, name, err := BuildRemoteCommand(Opts{Agent: "claude", Task: "hi"})
	if err != nil {
		t.Fatalf("BuildRemoteCommand: %v", err)
	}
	if !strings.HasPrefix(name, "cc-claude-") {
		t.Errorf("generated name = %q, want prefix cc-claude-", name)
	}
	// An empty --dir omits -c entirely: ssh already lands in $HOME, and a quoted "~" is not
	// expanded by sh, so emitting -c '~' would make tmux try to chdir into a literal "~" directory.
	if strings.Contains(cmd, "-c ") {
		t.Errorf("default dir should omit -c, got: %s", cmd)
	}
	if !strings.Contains(cmd, `"${SHELL:-/bin/sh}" -lc`) {
		t.Errorf("expected the agent to run under the login shell: %s", cmd)
	}
}

func TestDirFlag(t *testing.T) {
	cases := map[string]string{
		"":           "",
		"~":          `-c "$HOME"`,
		"~/my repo":  `-c "$HOME"/'my repo'`,
		"/tmp/work":  `-c '/tmp/work'`,
		"relative/p": `-c 'relative/p'`,
	}
	for dir, want := range cases {
		if got := dirFlag(dir); got != want {
			t.Errorf("dirFlag(%q) = %q, want %q", dir, got, want)
		}
	}
}

func TestBuildRemoteCommand_TildeDirUsesHOME(t *testing.T) {
	cmd, _, err := BuildRemoteCommand(Opts{Agent: "codex", Dir: "~/my repo", Task: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cmd, `-c "$HOME"/'my repo'`) {
		t.Errorf("expected $HOME-relative -c flag, got: %s", cmd)
	}
	// Must be valid sh: the adjacent "$HOME"/'my repo' concatenation is the whole point.
	if out, err := exec.Command("sh", "-n", "-c", cmd).CombinedOutput(); err != nil {
		t.Fatalf("sh -n rejected tilde-dir command: %v\n%s", err, out)
	}
}

func TestBuildRemoteCommand_KeepsSessionAliveAfterAgentExits(t *testing.T) {
	cmd, _, err := BuildRemoteCommand(Opts{Agent: "claude", Task: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cmd, `exec "${SHELL:-/bin/sh}" -l'`) {
		t.Errorf("expected the session to exec into a login shell after the agent exits: %s", cmd)
	}
}

func TestBuildRemoteCommand_NeverExecutesTask(t *testing.T) {
	// A task payload that would create a marker file if it were ever interpreted instead of quoted.
	dir := t.TempDir()
	marker := dir + "/pwned"
	cmd, _, err := BuildRemoteCommand(Opts{
		Agent: "claude",
		Name:  "cc-claude-safe",
		Task:  "$(touch " + marker + ")",
	})
	if err != nil {
		t.Fatalf("BuildRemoteCommand: %v", err)
	}
	// Run the generated command for real, but stub out tmux so nothing actually launches an agent;
	// the only thing under test is whether the shell ever expands/executes the embedded payload.
	stubDir := t.TempDir()
	writeStubTmux(t, stubDir)
	run := exec.Command("sh", "-c", cmd)
	run.Env = append(run.Environ(), "PATH="+stubDir)
	if out, err := run.CombinedOutput(); err != nil {
		t.Fatalf("generated command failed: %v\n%s", err, out)
	}
	if _, err := exec.Command("test", "-e", marker).CombinedOutput(); err == nil {
		t.Fatalf("task payload was executed by the shell instead of passed as data: marker created")
	}
}

// writeStubTmux puts a no-op `tmux` script on dir so BuildRemoteCommand's output can run without
// a real tmux server; it echoes its argv so callers can assert on invocation shape if needed.
func writeStubTmux(t *testing.T, dir string) {
	t.Helper()
	path := dir + "/tmux"
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("writeStubTmux: %v", err)
	}
}

// writeFailingNewSessionTmux puts a `tmux` stub on dir that fails `new-session` (simulating a
// duplicate session name) but succeeds on every other subcommand, appending every invocation's
// subcommand to logPath so the test can tell whether `set-option` ran despite the failure.
func writeFailingNewSessionTmux(t *testing.T, dir, logPath string) {
	t.Helper()
	script := "#!/bin/sh\necho \"$1\" >> " + logPath + "\nif [ \"$1\" = new-session ]; then exit 1; fi\nexit 0\n"
	if err := os.WriteFile(dir+"/tmux", []byte(script), 0o755); err != nil {
		t.Fatalf("writeFailingNewSessionTmux: %v", err)
	}
}

// TestBuildRemoteCommand_TaggingSkippedWhenNewSessionFails is the regression test for Appendix A
// codex-review finding #10 (internal/dispatch/dispatch.go:137): a duplicate session name makes
// `tmux new-session` fail, and the generated script must not then tag whatever unrelated session
// already holds that name with this dispatch's @cc_agent metadata.
func TestBuildRemoteCommand_TaggingSkippedWhenNewSessionFails(t *testing.T) {
	cmd, name, err := BuildRemoteCommand(Opts{Agent: "claude", Name: "cc-claude-dup", Task: "hi"})
	if err != nil {
		t.Fatalf("BuildRemoteCommand: %v", err)
	}
	stubDir := t.TempDir()
	logPath := t.TempDir() + "/calls.log"
	writeFailingNewSessionTmux(t, stubDir, logPath)

	run := exec.Command("sh", "-c", cmd)
	run.Env = append(run.Environ(), "PATH="+stubDir)
	out, runErr := run.CombinedOutput()
	if runErr == nil {
		t.Fatalf("generated command must fail when tmux new-session fails for %q; output=%s", name, out)
	}

	logged, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("reading tmux call log: %v", readErr)
	}
	calls := strings.Fields(string(logged))
	for _, c := range calls {
		if c == "set-option" {
			t.Fatalf("tmux set-option ran despite new-session failing; calls=%v", calls)
		}
	}
	if len(calls) == 0 || calls[0] != "new-session" {
		t.Fatalf("expected new-session to be the (only) tmux call attempted, got %v", calls)
	}
}

func TestSSHArgs_IsArgvNotShellString(t *testing.T) {
	// SSHArgs always wraps the remote command as `sh -c <quoted>` so it runs under POSIX sh
	// regardless of the remote login shell (e.g. fish).
	args := SSHArgs("myhost", "echo hi")
	want := []string{"ssh", "myhost", "sh -c 'echo hi'"}
	if len(args) != len(want) {
		t.Fatalf("SSHArgs = %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("SSHArgs[%d] = %q, want %q", i, args[i], want[i])
		}
	}
	// The host is its own argv element: a crafted host containing shell metacharacters must not
	// merge with the command when exec.Command passes args without a shell.
	args2 := SSHArgs("host; rm -rf /", "echo hi")
	if args2[1] != "host; rm -rf /" {
		t.Errorf("host argv element was mangled: %q", args2[1])
	}
	if len(args2) != 3 {
		t.Errorf("SSHArgs produced %d argv elements, want 3 (host stayed one element)", len(args2))
	}
	// The wrapped remote command must still be valid, injection-safe sh.
	if out, err := exec.Command("sh", "-n", "-c", args2[2]).CombinedOutput(); err != nil {
		t.Fatalf("sh -n rejected the wrapped remote command: %v\n%s", err, out)
	}
}

func TestAttachArgs(t *testing.T) {
	args := AttachArgs("myhost", "cc-claude-abcd")
	want := []string{"ssh", "-t", "myhost", "tmux", "attach", "-t", "cc-claude-abcd"}
	if len(args) != len(want) {
		t.Fatalf("AttachArgs = %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("AttachArgs[%d] = %q, want %q", i, args[i], want[i])
		}
	}
}

func TestListCommand_KnownFormat(t *testing.T) {
	want := `tmux list-sessions -F '#{session_name}|#{session_created}|#{session_attached}|#{pane_current_command}|#{@cc_agent}' 2>/dev/null | awk -F'|' '$1 ~ /^cc-/'`
	if got := ListCommand(); got != want {
		t.Errorf("ListCommand() = %q, want %q", got, want)
	}
	// It must itself be valid POSIX sh, since it's what gets sent over ssh.
	if out, err := exec.Command("sh", "-n", "-c", want).CombinedOutput(); err != nil {
		t.Fatalf("ListCommand is not valid sh: %v\n%s", err, out)
	}
}

func TestParseSessions(t *testing.T) {
	// Sample of the real tab-separated format produced by listCommand.
	input := "cc-claude-ab12|1700000000|1|claude|claude\n" +
		"cc-codex-ff00|1700000100|0|bash|codex\n"
	got := ParseSessions(input)
	if len(got) != 2 {
		t.Fatalf("ParseSessions returned %d sessions, want 2: %+v", len(got), got)
	}
	want := []Session{
		{Name: "cc-claude-ab12", Created: "1700000000", Attached: true, CurrentCmd: "claude", Agent: "claude"},
		{Name: "cc-codex-ff00", Created: "1700000100", Attached: false, CurrentCmd: "bash", Agent: "codex"},
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("ParseSessions()[%d] = %+v, want %+v", i, got[i], w)
		}
	}
}

// "" is also what `tmux list-sessions` with no server running produces: the error goes to stderr,
// which listCommand redirects away with 2>/dev/null, so awk sees empty input, not an error.
func TestParseSessions_EmptyIsEmptyNotError(t *testing.T) {
	for _, in := range []string{"", "\n"} {
		got := ParseSessions(in)
		if len(got) != 0 {
			t.Errorf("ParseSessions(%q) = %+v, want empty slice", in, got)
		}
	}
}

func TestParseSessions_ShortRowsDoNotPanic(t *testing.T) {
	got := ParseSessions("cc-claude-x\n")
	if len(got) != 1 || got[0].Name != "cc-claude-x" {
		t.Fatalf("ParseSessions short row = %+v", got)
	}
}

func TestDefaultName_MatchesValidName(t *testing.T) {
	for _, a := range Agents {
		n := DefaultName(a)
		if !ValidName(n) {
			t.Errorf("DefaultName(%q) = %q, fails ValidName", a, n)
		}
		if !strings.HasPrefix(n, "cc-"+a+"-") {
			t.Errorf("DefaultName(%q) = %q, want prefix cc-%s-", a, n, a)
		}
	}
}
