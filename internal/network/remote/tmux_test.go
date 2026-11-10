package remote

import (
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/textutil"
)

// shRoundTrip runs quoted through `sh -c 'printf %s <quoted>'`, proving it reproduces the bytes unexpanded.
func shRoundTrip(t *testing.T, quoted string) string {
	t.Helper()
	cmd := exec.Command("sh", "-c", "printf %s "+quoted)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("sh -c failed on quoted=%s: %v", quoted, err)
	}
	return string(out)
}

func TestShellQuote_RoundTrip(t *testing.T) {
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
			quoted := textutil.ShellQuote(in)
			got := shRoundTrip(t, quoted)
			if got != in {
				t.Fatalf("textutil.ShellQuote(%q) = %s; sh -c printf round-trip = %q, want %q", in, quoted, got, in)
			}
		})
	}
}

func TestShellQuote_NoInjectionViaEcho(t *testing.T) {
	// Embed a hostile payload as BuildCommand does; a created marker file would mean it executed.
	dir := t.TempDir()
	marker := dir + "/pwned"
	payload := "$(touch " + marker + ")"
	cmd := exec.Command("sh", "-c", "echo "+textutil.ShellQuote(payload)+" >/dev/null")
	if err := cmd.Run(); err != nil {
		t.Fatalf("sh -c failed: %v", err)
	}
	if _, err := exec.Command("test", "-e", marker).CombinedOutput(); err == nil {
		t.Fatalf("injection succeeded: marker file was created")
	}
}

func TestValidAgent(t *testing.T) {
	for _, a := range Agents {
		if !validAgent(a) {
			t.Errorf("validAgent(%q) = false, want true", a)
		}
	}
	for _, bad := range []string{"", "bash", "Claude", "claude ", " claude", "claude;rm"} {
		if validAgent(bad) {
			t.Errorf("validAgent(%q) = true, want false", bad)
		}
	}
}

func TestValidName(t *testing.T) {
	ok := []string{"a", "cc-claude-abcd", "A.B_C-9", strings.Repeat("x", 64)}
	for _, n := range ok {
		if !validName(n) {
			t.Errorf("ValidName(%q) = false, want true", n)
		}
	}
	bad := []string{"", "a b", "../x", strings.Repeat("x", 65), "a/b", "a;b", "a$b", "a\nb"}
	for _, n := range bad {
		if validName(n) {
			t.Errorf("ValidName(%q) = true, want false", n)
		}
	}
}

func TestRandIDLowerHex(t *testing.T) {
	for range 20 {
		got := randID()
		if len(got) != 8 || strings.Trim(got, "0123456789abcdef") != "" {
			t.Fatalf("randID() = %q, want eight lowercase hex digits", got)
		}
	}
}

// TestAgentLaunchArgs_Interactive pins agy's attached `-i=<task>` against copilot's separate `-i <task>`.
func TestAgentLaunchArgs_Interactive(t *testing.T) {
	if got := agentLaunchArgs("gemini", "hello"); len(got) != 1 || got[0] != "-i=hello" {
		t.Errorf("agentLaunchArgs(gemini) = %v, want [\"-i=hello\"]", got)
	}
	if got := agentLaunchArgs("copilot", "hello"); len(got) != 2 || got[0] != "-i" || got[1] != "hello" {
		t.Errorf("agentLaunchArgs(copilot) = %v, want [\"-i\" \"hello\"]", got)
	}
}

func TestBuildCommand_RejectsBadName(t *testing.T) {
	for _, bad := range []string{"a b", "../x", strings.Repeat("x", 65), " "} {
		if _, _, err := BuildCommand(Opts{Agent: "claude", Name: bad, Task: "hi"}); err == nil {
			t.Errorf("BuildCommand with --name %q: want error, got nil", bad)
		}
	}
}

func TestBuildCommand_RejectsUnknownAgent(t *testing.T) {
	for _, bad := range []string{"", "bash", "Claude", "claude;rm"} {
		if _, _, err := BuildCommand(Opts{Agent: bad, Task: "hi"}); err == nil {
			t.Errorf("BuildCommand with --agent %q: want error, got nil", bad)
		}
	}
}

func TestBuildCommand_SyntaxAndShape(t *testing.T) {
	for _, agent := range Agents {
		t.Run(agent, func(t *testing.T) {
			cmd, name, err := BuildCommand(Opts{
				Agent: agent,
				Dir:   `/tmp/a dir with "quotes" and spaces`,
				Name:  "cc-" + agent + "-test",
				Task:  `do something; with $(nasty) 'quotes' and "stuff" & | ; \n chars`,
			})
			if err != nil {
				t.Fatalf("BuildCommand: %v", err)
			}
			if name != "cc-"+agent+"-test" {
				t.Errorf("returned name = %q, want %q", name, "cc-"+agent+"-test")
			}
			// sh -n: syntax check only, nothing executes.
			check := exec.Command("sh", "-n", "-c", cmd)
			if out, err := check.CombinedOutput(); err != nil {
				t.Fatalf("sh -n rejected generated command: %v\n--- command ---\n%s\n--- output ---\n%s", err, cmd, out)
			}
			wantPrefix := "tmux new-session -d -s " + textutil.ShellQuote(name) + " -c " +
				textutil.ShellQuote(`/tmp/a dir with "quotes" and spaces`)
			if !strings.Contains(cmd, wantPrefix) {
				t.Errorf("command missing expected tmux invocation.\nwant substring: %s\ngot: %s", wantPrefix, cmd)
			}
		})
	}
}

func TestBuildCommand_DefaultDirAndName(t *testing.T) {
	cmd, name, err := BuildCommand(Opts{Agent: "claude", Task: "hi"})
	if err != nil {
		t.Fatalf("BuildCommand: %v", err)
	}
	if !strings.HasPrefix(name, "cc-claude-") {
		t.Errorf("generated name = %q, want prefix cc-claude-", name)
	}
	// An empty --dir omits -c: sh never expands a quoted "~", so tmux would chdir into a literal "~".
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

func TestBuildCommand_TildeDirUsesHOME(t *testing.T) {
	cmd, _, err := BuildCommand(Opts{Agent: "codex", Dir: "~/my repo", Task: "hi"})
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

func TestBuildCommand_KeepsSessionAliveAfterAgentExits(t *testing.T) {
	cmd, _, err := BuildCommand(Opts{Agent: "claude", Task: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cmd, `exec "${SHELL:-/bin/sh}" -l'`) {
		t.Errorf("expected the session to exec into a login shell after the agent exits: %s", cmd)
	}
}

func TestBuildCommand_NeverExecutesTask(t *testing.T) {
	// A task payload that would create a marker file if it were ever interpreted instead of quoted.
	dir := t.TempDir()
	marker := dir + "/pwned"
	cmd, _, err := BuildCommand(Opts{
		Agent: "claude",
		Name:  "cc-claude-safe",
		Task:  "$(touch " + marker + ")",
	})
	if err != nil {
		t.Fatalf("BuildCommand: %v", err)
	}
	// Run the generated command with tmux stubbed; only whether the shell expands the payload is under test.
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

// writeStubTmux puts a no-op `tmux` on dir that echoes its argv, so BuildCommand's output runs serverless.
func writeStubTmux(t *testing.T, dir string) {
	t.Helper()
	path := dir + "/tmux"
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("writeStubTmux: %v", err)
	}
}

// writeFailingNewSessionTmux fails `new-session` only and logs every subcommand to logPath.
func writeFailingNewSessionTmux(t *testing.T, dir, logPath string) {
	t.Helper()
	script := "#!/bin/sh\necho \"$1\" >> " + logPath + "\nif [ \"$1\" = new-session ]; then exit 1; fi\nexit 0\n"
	if err := os.WriteFile(dir+"/tmux", []byte(script), 0o755); err != nil {
		t.Fatalf("writeFailingNewSessionTmux: %v", err)
	}
}

// TestBuildCommand_TaggingSkippedWhenNewSessionFails: a duplicate name must not tag the other session.
func TestBuildCommand_TaggingSkippedWhenNewSessionFails(t *testing.T) {
	cmd, name, err := BuildCommand(Opts{Agent: "claude", Name: "cc-claude-dup", Task: "hi"})
	if err != nil {
		t.Fatalf("BuildCommand: %v", err)
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

func TestExecArgs_RemoteIsArgvNotShellString(t *testing.T) {
	// ExecArgs wraps the remote command as base64 under `sh -c` so it runs under POSIX sh whatever the login shell.
	t.Setenv(ReuseEnv, "0")
	args, err := ExecArgs("myhost", "echo hi", false, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ssh", "-o", "ServerAliveInterval=15", "--", "myhost", encodeScript("echo hi")}
	if !slices.Equal(args, want) {
		t.Fatalf("ExecArgs = %q, want %q", args, want)
	}
	// The host is its own argv element: shell metacharacters must not merge with the command.
	args2, err := ExecArgs("host;id", "echo hi", false, false)
	if err != nil || len(args2) != 6 || args2[4] != "host;id" {
		t.Fatalf("host argv element was mangled: %q (%v)", args2, err)
	}
	if _, err := ExecArgs("host; id", "echo hi", false, false); err == nil {
		t.Error("ExecArgs accepted a host with whitespace")
	}
	// The wrapped remote command must run under sh and reproduce the script.
	out, err := exec.Command("sh", "-c", args2[5]).CombinedOutput()
	if err != nil || string(out) != "hi\n" {
		t.Fatalf("sh ran the wrapped remote command: %q, %v", out, err)
	}
	if local, _ := ExecArgs("localhost", "echo hi", true, true); !slices.Equal(local, []string{"sh", "-c", "echo hi"}) {
		t.Errorf("local ExecArgs = %q", local)
	}
}

func TestAttachArgs(t *testing.T) {
	t.Setenv(ReuseEnv, "off")
	args, err := attachArgs("myhost", "cc-claude-abcd", false)
	want := []string{"ssh", "-o", "ServerAliveInterval=15", "-t", "--", "myhost",
		encodeScript("tmux attach -t 'cc-claude-abcd'")}
	if err != nil || !slices.Equal(args, want) {
		t.Fatalf("AttachArgs = %q (%v), want %q", args, err, want)
	}
	local, _ := attachArgs("myhost", "cc-claude-abcd", true)
	if !slices.Equal(local, []string{"tmux", "attach", "-t", "cc-claude-abcd"}) {
		t.Errorf("local AttachArgs = %q", local)
	}
	if got := AttachCommand("myhost", "cc-claude-abcd", false); got != "ssh -t -- myhost tmux attach -t cc-claude-abcd" {
		t.Errorf("AttachCommand = %q", got)
	}
}

func TestListCommand_KnownFormat(t *testing.T) {
	want := `out=$(tmux list-sessions -F ` +
		`'#{session_name}|#{session_created}|#{session_attached}|#{pane_current_command}|#{@cc_agent}' 2>&1) || ` +
		`{ case $out in *"no server running"*|*"No such file or directory"*) out= ;; ` +
		`*) printf '%s\n' "$out" >&2; exit 1 ;; esac; }; printf '%s\n' "$out" | awk -F'|' '$1 ~ /^cc-/'`
	if listCommand != want {
		t.Errorf("listCommand = %q, want %q", listCommand, want)
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
	got := parseSessions(input)
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

// "" is also what list-sessions prints with no server (stderr is discarded), so empty is not an error.
func TestParseSessions_EmptyIsEmptyNotError(t *testing.T) {
	for _, in := range []string{"", "\n"} {
		got := parseSessions(in)
		if len(got) != 0 {
			t.Errorf("ParseSessions(%q) = %+v, want empty slice", in, got)
		}
	}
}

func TestParseSessions_ShortRowsDoNotPanic(t *testing.T) {
	got := parseSessions("cc-claude-x\n")
	if len(got) != 1 || got[0].Name != "cc-claude-x" {
		t.Fatalf("ParseSessions short row = %+v", got)
	}
}

func TestDefaultName_MatchesValidName(t *testing.T) {
	for _, a := range Agents {
		n := defaultName(a)
		if !validName(n) {
			t.Errorf("DefaultName(%q) = %q, fails ValidName", a, n)
		}
		if !strings.HasPrefix(n, "cc-"+a+"-") {
			t.Errorf("DefaultName(%q) = %q, want prefix cc-%s-", a, n, a)
		}
	}
}
