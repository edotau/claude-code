package remote

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"

	"github.com/edotau/claude-code/internal/textutil"
)

// hermeticDispatch isolates tailscale credentials so ResolveHost passes the host through, never a real tailnet.
func hermeticDispatch(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, n := range []string{"TAILSCALE_OAUTH_CLIENT_ID", "TAILSCALE_OAUTH_CLIENT_SECRET", "TAILSCALE_API_KEY"} {
		t.Setenv(n, "")
	}
}

// emptyPATH points PATH at an empty directory: no ssh, no tailscale, no tmux resolvable.
func emptyPATH(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

// fakeSSHDecode opens a fake ssh script: it decodes the base64 remote script ssh's last argument carries into $script.
const fakeSSHDecode = "#!/bin/sh\nfor a; do last=$a; done\nb64=${last#*'printf %s '}; b64=${b64%%' |'*}\n" +
	"script=$(printf %s \"$b64\" | /usr/bin/base64 -d)\n"

// decodedRemoteScript recovers the base64 script from a dry-run's printed ssh argv.
func decodedRemoteScript(t *testing.T, out string) string {
	t.Helper()
	_, rest, ok := strings.Cut(out, "printf %s ")
	b64, _, ok2 := strings.Cut(rest, " | base64 -d")
	b, err := base64.StdEncoding.DecodeString(b64)
	if !ok || !ok2 || err != nil {
		t.Fatalf("no base64 script in dry-run output (%v):\n%s", err, out)
	}
	return string(b)
}

// fakeSSHOnPATH puts an `ssh` script on PATH that returns canned output for --list's tmux listing.
func fakeSSHOnPATH(t *testing.T, listOutput string) { fakeSSH(t, "list-sessions", listOutput) }

// fakeSSHStatusOnPATH puts an `ssh` on PATH answering the --status pane-capture command with panes.
func fakeSSHStatusOnPATH(t *testing.T, panes string) { fakeSSH(t, "list-panes", panes) }

// fakeSSH puts an `ssh` on PATH that prints output for the remote script running `tmux <tmuxCmd>`.
func fakeSSH(t *testing.T, tmuxCmd, output string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	dir := t.TempDir()
	script := fakeSSHDecode + "case \"$script\" in *tmux\\ " + tmuxCmd + "*) printf '%s' " +
		textutil.ShellQuote(output) + " ;; esac\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func TestDispatchMissingHost(t *testing.T) {
	hermeticDispatch(t)
	code, _, stderr := hosttest.CaptureOutput(t, func() int { return Cmd([]string{"hi"}) })
	if code == 0 {
		t.Fatalf("missing --host: code = 0, want nonzero")
	}
	if !strings.Contains(stderr, "--host") {
		t.Errorf("stderr = %q, want it to mention --host", stderr)
	}
}

func TestDispatchConflictingModes(t *testing.T) {
	hermeticDispatch(t)
	fakeSSHOnPATH(t, "cc-claude-ab12|1700000000|1|claude|claude\n")
	code, out, stderr := hosttest.CaptureOutput(t, func() int {
		return Cmd([]string{"--host", "myhost", "--list", "--attach", "cc-claude-ab12"})
	})
	if code != 2 || out != "" || !strings.Contains(stderr, "mutually exclusive") {
		t.Fatalf("conflicting modes: code=%d out=%q stderr=%q", code, out, stderr)
	}
}

func TestDispatchMissingTask(t *testing.T) {
	hermeticDispatch(t)
	emptyPATH(t)
	code, _, stderr := hosttest.CaptureOutput(t, func() int { return Cmd([]string{"--host", "myhost"}) })
	if code == 0 {
		t.Fatalf("missing task: code = 0, want nonzero")
	}
	if !strings.Contains(stderr, "task") {
		t.Errorf("stderr = %q, want it to mention task", stderr)
	}
}

func TestDispatchUnknownAgent(t *testing.T) {
	hermeticDispatch(t)
	emptyPATH(t)
	code, _, stderr := hosttest.CaptureOutput(t, func() int {
		return Cmd([]string{"--host", "myhost", "--agent", "bash", "hi"})
	})
	if code == 0 {
		t.Fatalf("unknown --agent: code = 0, want nonzero")
	}
	if !strings.Contains(stderr, "--agent") {
		t.Errorf("stderr = %q, want it to mention --agent", stderr)
	}
}

// TestDispatchDryRunSpawnsNoSSH: with no ssh on PATH, exit 0 proves --dry-run never ran the ssh round trip.
func TestDispatchDryRunSpawnsNoSSH(t *testing.T) {
	hermeticDispatch(t)
	emptyPATH(t)
	code, out, stderr := hosttest.CaptureOutput(t, func() int {
		return Cmd([]string{"--host", "myhost", "--dir", "/tmp/work dir", "--name", "cc-claude-abcd",
			"--dry-run", "do", "the", "thing"})
	})
	if code != 0 {
		t.Fatalf("dry-run exit %d, want 0; stderr=%q", code, stderr)
	}
	if !strings.Contains(out, "argv[0]: ssh") {
		t.Errorf("dry-run output missing argv[0] line:\n%s", out)
	}
	if !regexp.MustCompile(`argv\[\d+\]: --\n\s*argv\[\d+\]: myhost\n`).MatchString(out) {
		t.Errorf("dry-run output missing resolved host right after --:\n%s", out)
	}
	if !strings.Contains(out, "attach: ssh -t -- myhost tmux attach -t cc-claude-abcd") {
		t.Errorf("dry-run output missing attach hint:\n%s", out)
	}
	// The task given as multiple positional words must be joined and passed as one shell-quoted unit.
	if !strings.Contains(decodedRemoteScript(t, out), "'do the thing'") {
		t.Errorf("dry-run output does not show the joined, quoted task:\n%s", out)
	}
}

func TestDispatchDryRunRejectsBadName(t *testing.T) {
	hermeticDispatch(t)
	emptyPATH(t)
	code, _, stderr := hosttest.CaptureOutput(t, func() int {
		return Cmd([]string{"--host", "myhost", "--name", "a b", "--dry-run", "hi"})
	})
	if code == 0 {
		t.Fatalf("bad --name: code = 0, want nonzero")
	}
	if !strings.Contains(stderr, "--name") {
		t.Errorf("stderr = %q, want it to mention --name", stderr)
	}
}

func TestDispatchListTextAndJSON(t *testing.T) {
	hermeticDispatch(t)
	listing := "cc-claude-ab12|1700000000|1|claude|claude\n"
	fakeSSHOnPATH(t, listing)
	code, out, stderr := hosttest.CaptureOutput(t, func() int {
		return Cmd([]string{"--list", "--host",
			"myhost"})
	})
	if code != 0 {
		t.Fatalf("--list exit %d, want 0; stderr=%q", code, stderr)
	}
	if !strings.Contains(out, "cc-claude-ab12") {
		t.Errorf("--list text output missing session name:\n%s", out)
	}
	if !strings.Contains(out, "cmd=claude") {
		t.Errorf("--list text output missing parsed CurrentCmd field (fieldSep regression):\n%s", out)
	}

	code, out, stderr = hosttest.CaptureOutput(t, func() int {
		return Cmd([]string{"--list", "--host", "myhost", "--format", "json"})
	})
	if code != 0 {
		t.Fatalf("--list --format json exit %d, want 0; stderr=%q", code, stderr)
	}
	if !strings.Contains(out, `"Name"`) || !strings.Contains(out, "cc-claude-ab12") {
		t.Errorf("--list json output = %q, want it to contain the session as JSON", out)
	}
}

func TestDispatchListEmptyIsNotAnError(t *testing.T) {
	hermeticDispatch(t)
	fakeSSHOnPATH(t, "")
	code, out, stderr := hosttest.CaptureOutput(t, func() int {
		return Cmd([]string{"--list", "--host",
			"myhost"})
	})
	if code != 0 {
		t.Fatalf("--list (no sessions) exit %d, want 0; stderr=%q", code, stderr)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("--list (no sessions) output = %q, want empty", out)
	}
}

func TestDispatchAttachRejectsBadSessionName(t *testing.T) {
	hermeticDispatch(t)
	emptyPATH(t)
	code, _, stderr := hosttest.CaptureOutput(t, func() int {
		return Cmd([]string{"--attach", "--host", "myhost", "../etc"})
	})
	if code == 0 {
		t.Fatalf("--attach with bad session name: code = 0, want nonzero")
	}
	if !strings.Contains(stderr, "session") {
		t.Errorf("stderr = %q, want it to mention session", stderr)
	}
}

func TestDispatchAttachMissingSessionArg(t *testing.T) {
	hermeticDispatch(t)
	emptyPATH(t)
	code, _, stderr := hosttest.CaptureOutput(t, func() int {
		return Cmd([]string{"--attach", "--host", "myhost"})
	})
	if code == 0 {
		t.Fatalf("--attach with no session name: code = 0, want nonzero")
	}
	if !strings.Contains(stderr, "session name") {
		t.Errorf("stderr = %q, want it to mention session name", stderr)
	}
}

// statusCaptureFrame builds one record in captureCommand's on-wire format so the fake ssh drives the real parser.
func statusCaptureFrame(sess, pid, agent, cmd, text string) string {
	return "\x01" + sess + "|" + pid + "|" + agent + "|" + cmd + "\n" + text + "\x02\n"
}

func TestDispatchListStatusTextAndJSON(t *testing.T) {
	hermeticDispatch(t)
	capture := statusCaptureFrame("cc-claude-ab12", "%1", "claude", "claude", "thinking...")
	fakeSSHStatusOnPATH(t, capture)
	code, out, stderr := hosttest.CaptureOutput(t, func() int {
		return Cmd([]string{"--list", "--host", "myhost", "--status"})
	})
	if code != 0 {
		t.Fatalf("--list --status exit %d, want 0; stderr=%q", code, stderr)
	}
	if !strings.Contains(out, "cc-claude-ab12") || !strings.Contains(out, "state=working") {
		t.Errorf("--list --status text output missing session/state:\n%s", out)
	}

	code, out, stderr = hosttest.CaptureOutput(t, func() int {
		return Cmd([]string{"--list", "--host", "myhost", "--status", "--format", "json"})
	})
	if code != 0 {
		t.Fatalf("--list --status --format json exit %d, want 0; stderr=%q", code, stderr)
	}
	if !strings.Contains(out, `"state": "working"`) {
		t.Errorf("--list --status json output = %q, want a classified working state", out)
	}
}

func TestDispatchListStatusNoPanesIsEmptyNotError(t *testing.T) {
	hermeticDispatch(t)
	fakeSSHStatusOnPATH(t, "")
	code, out, stderr := hosttest.CaptureOutput(t, func() int {
		return Cmd([]string{"--list", "--host", "myhost", "--status"})
	})
	if code != 0 {
		t.Fatalf("--list --status (no panes) exit %d, want 0; stderr=%q", code, stderr)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("--list --status (no panes) output = %q, want empty", out)
	}
}

func TestDispatchListStatusSSHFailure(t *testing.T) {
	hermeticDispatch(t)
	emptyPATH(t) // no ssh on PATH at all: the capture round trip itself fails
	code, _, stderr := hosttest.CaptureOutput(t, func() int {
		return Cmd([]string{"--list", "--host", "myhost", "--status"})
	})
	if code == 0 {
		t.Fatalf("--list --status with no ssh available: code = 0, want nonzero")
	}
	if !strings.Contains(stderr, "dispatch") {
		t.Errorf("stderr = %q, want it to report the capture failure", stderr)
	}
}

// TestDispatchLocalhostDryRunNeverLaunches: localhost runs under sh -c, and --dry-run must never invoke the stubs.
func TestDispatchLocalhostDryRunNeverLaunches(t *testing.T) {
	hermeticDispatch(t)
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	for _, name := range []string{"sh", "tmux"} {
		script := "#!/bin/sh\ntouch " + textutil.ShellQuote(marker) + "\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	code, out, stderr := hosttest.CaptureOutput(t, func() int {
		return Cmd([]string{"--host", "localhost", "task", "--dry-run"})
	})
	if code != 0 {
		t.Fatalf("dry-run exit %d, want 0; stderr=%q", code, stderr)
	}
	if !strings.Contains(out, "argv[0]: sh") {
		t.Errorf("dry-run output missing argv[0]: sh:\n%s", out)
	}
	if !strings.Contains(out, "'task'") {
		t.Errorf("dry-run output missing the quoted task:\n%s", out)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("marker exists — a stubbed sh/tmux was actually launched, stat err=%v", err)
	}
}

func TestDispatchHelp(t *testing.T) {
	code, _, stderr := hosttest.CaptureOutput(t, func() int { return Cmd(nil) })
	if code != 0 {
		t.Fatalf("no args: code = %d, want 0", code)
	}
	if !strings.Contains(stderr, "usage: claude-code network remote") {
		t.Errorf("stderr = %q, want usage text", stderr)
	}
}

// TestDispatchListJSONWriteError: --list --format json on a closed stdout exits non-zero.
func TestDispatchListJSONWriteError(t *testing.T) {
	hermeticDispatch(t)
	fakeSSHOnPATH(t, "cc-claude-ab12|1700000000|1|claude|claude\n")
	hosttest.ClosedStdout(t)

	code := Cmd([]string{"--list", "--host", "myhost", "--format", "json"})
	if code == 0 {
		t.Errorf("dispatch --list --format json on closed stdout: exit %d, want non-zero", code)
	}
}

// TestDispatchRejectsFlagFromAnotherMode: --dry-run belongs to start mode, so --list refuses it.

func TestDispatchStartReportsStdoutFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	hermeticDispatch(t)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte("#!/bin/sh\nprintf 'remote output\\n'\n"),
		0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	hosttest.ClosedStdout(t)
	if got := dispatchStart(context.Background(), "stdout-test.invalid", "", "claude", "cc-stdout-test", "hello",
		false); got == 0 {
		t.Fatal("dispatch start exit = 0 on closed stdout, want nonzero")
	}
}

// An option-shaped --host (ssh would run its ProxyCommand) is refused before any ssh argv is built.
func TestNetworkRemoteRefusesOptionShapedHost(t *testing.T) {
	hermeticDispatch(t)
	emptyPATH(t)
	code, out, stderr := hosttest.CaptureOutput(t, func() int {
		return Cmd([]string{"--host", "-oProxyCommand=touch /tmp/pwned", "--dry-run", "x"})
	})
	if code == 0 || !strings.Contains(stderr, "invalid ssh host") || strings.Contains(out, "ProxyCommand") {
		t.Fatalf("exit %d, stdout %q, stderr %q; want a refusal naming the invalid host", code, out, stderr)
	}
}

func TestRemoteTextReportsStdoutFailure(t *testing.T) {
	for _, status := range []bool{false, true} {
		t.Run(fmt.Sprintf("status_%t", status), func(t *testing.T) {
			hermeticDispatch(t)
			args := []string{"--list", "--host", "myhost"}
			want := "network remote --list:"
			if status {
				fakeSSHStatusOnPATH(t, statusCaptureFrame("cc-claude-ab12", "%1", "claude", "claude", "thinking..."))
				args = append(args, "--status")
				want = "network remote --status:"
			} else {
				fakeSSHOnPATH(t, "cc-claude-ab12|1700000000|1|claude|claude\n")
			}
			code, stderr := hosttest.CaptureClosedStdout(t, func() int { return Cmd(args) })
			if code != 1 || !strings.Contains(stderr, want) || !strings.Contains(stderr, "file already closed") {
				t.Fatalf("failed text output reported success: exit=%d stderr=%q", code, stderr)
			}
		})
	}
}
