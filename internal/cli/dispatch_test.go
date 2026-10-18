package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// hermeticDispatch isolates tailscale credentials/config so ResolveHost falls through to its
// pass-the-host-through branch instead of hitting a real tailnet.
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
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	t.Setenv("PATH", t.TempDir())
}

// fakeSSHOnPATH puts an `ssh` script on PATH that returns canned output for --list's tmux listing.
func fakeSSHOnPATH(t *testing.T, listOutput string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"case \"$2\" in *tmux\\ list-sessions*) printf '%s' " + shQuote(listOutput) + " ;; esac\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func TestDispatchMissingHost(t *testing.T) {
	hermeticDispatch(t)
	code, _, stderr := captureOutput(t, func() int { return cmdDispatch([]string{"hi"}) })
	if code == 0 {
		t.Fatalf("missing --host: code = 0, want nonzero")
	}
	if !strings.Contains(stderr, "--host") {
		t.Errorf("stderr = %q, want it to mention --host", stderr)
	}
}

func TestDispatchMissingTask(t *testing.T) {
	hermeticDispatch(t)
	emptyPATH(t)
	code, _, stderr := captureOutput(t, func() int { return cmdDispatch([]string{"--host", "myhost"}) })
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
	code, _, stderr := captureOutput(t, func() int {
		return cmdDispatch([]string{"--host", "myhost", "--agent", "bash", "hi"})
	})
	if code == 0 {
		t.Fatalf("unknown --agent: code = 0, want nonzero")
	}
	if !strings.Contains(stderr, "--agent") {
		t.Errorf("stderr = %q, want it to mention --agent", stderr)
	}
}

// TestDispatchDryRunSpawnsNoSSH is the security-relevant assertion: PATH has no ssh binary at all,
// so if --dry-run ever reached exec.CommandContext it would fail with "executable file not found".
// Exit 0 here proves the ssh/tmux round trip never happened.
func TestDispatchDryRunSpawnsNoSSH(t *testing.T) {
	hermeticDispatch(t)
	emptyPATH(t)
	code, out, stderr := captureOutput(t, func() int {
		return cmdDispatch([]string{"--host", "myhost", "--dir", "/tmp/work dir", "--name", "cc-claude-abcd", "--dry-run", "do", "the", "thing"})
	})
	if code != 0 {
		t.Fatalf("dry-run exit %d, want 0; stderr=%q", code, stderr)
	}
	if !strings.Contains(out, "argv[0]: ssh") {
		t.Errorf("dry-run output missing argv[0] line:\n%s", out)
	}
	if !strings.Contains(out, "argv[1]: myhost") {
		t.Errorf("dry-run output missing resolved host in argv[1]:\n%s", out)
	}
	if !strings.Contains(out, "attach: ssh -t myhost tmux attach -t cc-claude-abcd") {
		t.Errorf("dry-run output missing attach hint:\n%s", out)
	}
	// The task given as multiple positional words must be joined and passed as one shell-quoted unit.
	if !strings.Contains(out, "'do the thing'") {
		t.Errorf("dry-run output does not show the joined, quoted task:\n%s", out)
	}
}

func TestDispatchDryRunRejectsBadName(t *testing.T) {
	hermeticDispatch(t)
	emptyPATH(t)
	code, _, stderr := captureOutput(t, func() int {
		return cmdDispatch([]string{"--host", "myhost", "--name", "a b", "--dry-run", "hi"})
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
	listing := "cc-claude-ab12\t1700000000\t1\tclaude\n"
	fakeSSHOnPATH(t, listing)
	code, out, stderr := captureOutput(t, func() int { return cmdDispatch([]string{"--list", "--host", "myhost"}) })
	if code != 0 {
		t.Fatalf("--list exit %d, want 0; stderr=%q", code, stderr)
	}
	if !strings.Contains(out, "cc-claude-ab12") {
		t.Errorf("--list text output missing session name:\n%s", out)
	}

	code, out, stderr = captureOutput(t, func() int {
		return cmdDispatch([]string{"--list", "--host", "myhost", "--format", "json"})
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
	code, out, stderr := captureOutput(t, func() int { return cmdDispatch([]string{"--list", "--host", "myhost"}) })
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
	code, _, stderr := captureOutput(t, func() int {
		return cmdDispatch([]string{"--attach", "--host", "myhost", "../etc"})
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
	code, _, stderr := captureOutput(t, func() int {
		return cmdDispatch([]string{"--attach", "--host", "myhost"})
	})
	if code == 0 {
		t.Fatalf("--attach with no session name: code = 0, want nonzero")
	}
	if !strings.Contains(stderr, "session name") {
		t.Errorf("stderr = %q, want it to mention session name", stderr)
	}
}

func TestDispatchHelp(t *testing.T) {
	code, _, stderr := captureOutput(t, func() int { return cmdDispatch(nil) })
	if code != 2 {
		t.Fatalf("no args: code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "usage: claude-code dispatch") {
		t.Errorf("stderr = %q, want usage text", stderr)
	}
}
