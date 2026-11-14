package remote

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeConn is an ssh that printed out and, once stopped, reports stderr.
func fakeConn(out, stderr string, stopped *atomic.Int32) *sshConn {
	return &sshConn{
		out:    bufio.NewReader(strings.NewReader(out)),
		stderr: func() string { return stderr },
		stop:   func() { stopped.Add(1) },
	}
}

// fakeSSH swaps startSSH for script, which sees each argv in turn and returns that call's connection.
func swapStartSSH(t *testing.T, script func(call int, argv []string) (*sshConn, error)) *[][]string {
	t.Helper()
	var argvs [][]string
	old := startSSH
	startSSH = func(_ context.Context, argv []string) (*sshConn, error) {
		argvs = append(argvs, argv)
		return script(len(argvs), argv)
	}
	t.Cleanup(func() { startSSH = old })
	return &argvs
}

func TestReverseForwardUp(t *testing.T) {
	var stopped atomic.Int32
	argvs := swapStartSSH(t, func(int, []string) (*sshConn, error) { return fakeConn(forwardReady+"\n", "", &stopped), nil })
	port, stop, err := ReverseForward(context.Background(), "wks", 8030, 8050, 8031)
	if err != nil || port != 8030 {
		t.Fatalf("port %d err %v, want 8030 and none", port, err)
	}
	argv := (*argvs)[0]
	if i := slices.Index(argv, "-R"); i < 0 || argv[i+1] != "8030:localhost:8031" {
		t.Errorf("want -R 8030:localhost:8031: %q", argv)
	}
	for _, want := range []string{"ExitOnForwardFailure=yes", "ControlPath=none", "BatchMode=yes", "ConnectTimeout=10"} {
		if !slices.Contains(argv, want) {
			t.Errorf("argv lacks %s: %q", want, argv)
		}
	}
	if i := slices.Index(argv, "--"); i < 0 || argv[i+1] != "wks" {
		t.Errorf("host must follow --: %q", argv)
	}
	if stopped.Load() != 0 {
		t.Error("forward closed before stop")
	}
	stop()
	if stopped.Load() != 1 {
		t.Errorf("stop closed the forward %d times, want 1", stopped.Load())
	}
}

const busyMsg = "Error: remote port forwarding failed for listen port 8030\n"

func TestReverseForwardTakenPortMovesOn(t *testing.T) {
	var stopped atomic.Int32
	argvs := swapStartSSH(t, func(call int, _ []string) (*sshConn, error) {
		if call == 1 {
			return fakeConn("", busyMsg, &stopped), nil
		}
		return fakeConn(forwardReady+"\n", "", &stopped), nil
	})
	port, _, err := ReverseForward(context.Background(), "wks", 8030, 8050, 8030)
	if err != nil || port != 8031 {
		t.Fatalf("port %d err %v, want 8031 after 8030 was taken", port, err)
	}
	if len(*argvs) != 2 || stopped.Load() != 1 {
		t.Errorf("%d ssh calls, %d closed; want 2 calls and the failed one closed", len(*argvs), stopped.Load())
	}
}

func TestReverseForwardExactPortTakenNamesPortAndHost(t *testing.T) {
	var stopped atomic.Int32
	swapStartSSH(t, func(int, []string) (*sshConn, error) { return fakeConn("", busyMsg, &stopped), nil })
	_, _, err := ReverseForward(context.Background(), "wks", 8040, 8040, 8040)
	taken, ok := errors.AsType[*PortTakenError](err)
	if !ok || !strings.Contains(err.Error(), "port 8040 on wks") || taken.Host != "wks" {
		t.Errorf("err = %v, want a PortTakenError naming port 8040 and wks", err)
	}
}

func TestReverseForwardSSHFailureIsNotAPortError(t *testing.T) {
	var stopped atomic.Int32
	calls := swapStartSSH(t, func(int, []string) (*sshConn, error) {
		return fakeConn("", "\nssh: connect to host wks port 22: Connection refused\n", &stopped), nil
	})
	_, _, err := ReverseForward(context.Background(), "wks", 8030, 8050, 8030)
	if _, busy := errors.AsType[*PortTakenError](err); busy || err == nil ||
		!strings.Contains(err.Error(), "ssh to wks: ssh: connect to host wks port 22: Connection refused") {
		t.Errorf("err = %v, want an ssh error naming wks", err)
	}
	if len(*calls) != 1 {
		t.Errorf("%d ssh calls, want 1: an ssh failure never walks the ports", len(*calls))
	}
}

func TestReverseForwardRefusesOptionShapedHost(t *testing.T) {
	swapStartSSH(t, func(int, []string) (*sshConn, error) { t.Fatal("ssh started"); return nil, nil })
	if _, _, err := ReverseForward(context.Background(), "-oProxyCommand=x", 8030, 8030, 8030); err == nil {
		t.Error("an option-shaped host was accepted")
	}
}

func TestOpenURLOnSendsURLAsOneQuotedArgument(t *testing.T) {
	var got *exec.Cmd
	old := runCommand
	runCommand = func(c *exec.Cmd) error { got = c; return nil }
	t.Cleanup(func() { runCommand = old })
	const authURL = "https://x.cloud.databricks.com/oidc/v1/authorize?a=1&b=$(touch /tmp/p);'x"
	if err := OpenURLOn(context.Background(), "wks", authURL); err != nil {
		t.Fatal(err)
	}
	script := decodedScript(t, got.Args)
	want := "'databricks' 'open-url' 'https://x.cloud.databricks.com/oidc/v1/authorize?a=1&b=$(touch /tmp/p);'\\''x'"
	if !strings.HasSuffix(script, want) {
		t.Errorf("script = %q, want it to end with %q", script, want)
	}
	if strings.Contains(strings.Join(got.Args[:len(got.Args)-1], " "), "touch") {
		t.Errorf("the URL reached ssh outside the encoded script: %q", got.Args)
	}
}

func TestCheckLogin(t *testing.T) {
	login := []string{"databricks", "login", "--dry-run"}
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"remote alone", append([]string{"--remote", "a"}, login...), ""},
		{"host alone", append([]string{"--host", "a"}, login...), ""},
		{"remote then host", append([]string{"--remote", "a", "--host", "b"}, login...), "--remote with --host:"},
		{"host then remote", append([]string{"--host=b", "--remote=a"}, login...), "--remote with --host:"},
		{"host-browser", append(append([]string{"--remote", "a"}, login...), "--host-browser"), "--remote with --host-browser:"},
		{"host-browser with host only", append(append([]string{"--host", "a"}, login...), "--host-browser"), ""},
		{"other verb", []string{"--remote", "a", "--host", "b", "doctor"}, ""},
	}
	for _, c := range cases {
		err := CheckLogin(c.args, "--remote")
		if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
}

func TestPinLoginOnlyForLogin(t *testing.T) {
	t.Setenv(LoginRemoteEnv, "")
	if err := PinLogin("wks", []string{"doctor"}); err != nil || os.Getenv(LoginRemoteEnv) != "" {
		t.Errorf("a non-login verb pinned %q (err %v)", os.Getenv(LoginRemoteEnv), err)
	}
	if err := PinLogin("wks", []string{"databricks", "login"}); err != nil || os.Getenv(LoginRemoteEnv) != "wks" {
		t.Errorf("login pinned %q (err %v), want wks", os.Getenv(LoginRemoteEnv), err)
	}
}

// The real runner, on sh in place of ssh: the marker line arrives while the process lives, stop ends it, a failure's stderr is kept.
func TestStartSSHRunsAndStops(t *testing.T) {
	live, err := startSSH(context.Background(), []string{"sh", "-c", "echo " + forwardReady + "; exec cat >/dev/null"})
	if err != nil {
		t.Fatal(err)
	}
	if line, _ := live.out.ReadString('\n'); strings.TrimSpace(line) != forwardReady {
		t.Errorf("first line %q, want the marker", line)
	}
	live.stop()
	live.stop() // closing twice is harmless
	dead, err := startSSH(context.Background(), []string{"sh", "-c", "echo " + forwardFailed + " >&2; exit 255"})
	if err != nil {
		t.Fatal(err)
	}
	if line, _ := dead.out.ReadString('\n'); line != "" || !strings.Contains(dead.stderr(), forwardFailed) {
		t.Errorf("stdout %q stderr %q, want no stdout and the failure on stderr", line, dead.stderr())
	}
}
