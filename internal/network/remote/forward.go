package remote

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/textutil"
)

// LoginRemoteEnv carries the --remote host into a local `databricks login`, which signs in through that host's browser.
const LoginRemoteEnv = "HARNESS_LOGIN_REMOTE"

// PinLogin sets LoginRemoteEnv for a `databricks login` that `--remote host` runs here; any other verb is untouched.
func PinLogin(host string, rest []string) error {
	if !isLogin(rest) {
		return nil
	}
	return proc.Setenv(LoginRemoteEnv, host)
}

// LoginAutoEnv marks a login that CLAUDE_REMOTE_HOST sent through --remote, so login says why on its first line.
const LoginAutoEnv = "HARNESS_LOGIN_REMOTE_AUTO"

// autoSelf reports whether a host names this machine; tests swap it.
var autoSelf = IsSelf

// AutoLogin is the host a plain `databricks login` signs in through when CLAUDE_REMOTE_HOST names one; args follow argv[0].
// Only the variable decides: an explicit mode flag, a pinned login or a host naming this machine leaves the login local.
func AutoLogin(args []string) (host string, ok bool) {
	if !isLogin(args) || os.Getenv(LoginRemoteEnv) != "" {
		return "", false
	}
	if flagValues(args[2:])["as"] == "sp" || boolFlagOn(args[2:], "no-browser", HostBrowser[2:], "h", "help") {
		return "", false
	}
	host = autoEnvHost()
	if host == "" || host == Auto || namesSelf(host) {
		return "", false
	}
	return host, true
}

// namesSelf reports whether host, or its first DNS label, names this machine.
func namesSelf(host string) bool {
	if autoSelf(host) {
		return true
	}
	label, _, dotted := strings.Cut(host, ".")
	return dotted && !strings.Contains(host, "@") && autoSelf(label)
}

// CheckLogin refuses a `databricks login` that combines --remote with --host or --host-browser; flags are the global ones.
func CheckLogin(args []string, egressFlag string) error {
	seen := map[string]bool{}
	for len(args) > 0 {
		f, n := globalFlag(args, egressFlag)
		if n == 0 {
			break
		}
		seen[f], args = true, args[n:]
	}
	switch {
	case !isLogin(args) || !seen[egressFlag]:
		return nil
	case seen[Flag]:
		return fmt.Errorf("%s with %s: --remote signs in on this machine through the host's browser, "+
			"--host runs the whole login on the host", egressFlag, Flag)
	case boolFlagOn(args[2:], HostBrowser[2:]):
		return fmt.Errorf("%s with %s: --remote seals the token on this machine, "+
			"%s keeps the login and the token on the host", egressFlag, HostBrowser, HostBrowser)
	}
	return nil
}

// globalFlag reports which of --host and egressFlag starts args and how many arguments it takes (0 for neither).
func globalFlag(args []string, egressFlag string) (string, int) {
	for _, f := range []string{Flag, egressFlag} {
		if args[0] == f && len(args) > 1 {
			return f, 2
		}
		if strings.HasPrefix(args[0], f+"=") {
			return f, 1
		}
	}
	return "", 0
}

// PortTakenError is a reverse forward refused because the host's loopback ports are held or forwarding is not allowed.
type PortTakenError struct {
	Host        string
	First, Last int
}

func (e *PortTakenError) Error() string {
	if e.First == e.Last {
		return fmt.Sprintf("port %d on %s is taken (or its sshd forbids forwarding)", e.First, e.Host)
	}
	return fmt.Sprintf("ports %d-%d on %s are taken (or its sshd forbids forwarding)", e.First, e.Last, e.Host)
}

// ReverseForward makes localhost:<port> on host reach localhost:local here, port the first free one in first..last.
func ReverseForward(ctx context.Context, host string, first, last, local int) (port int, stop func(), err error) {
	for port = first; port <= last; port++ {
		stop, err = forwardOnce(ctx, host, port, local)
		if _, busy := errors.AsType[*PortTakenError](err); !busy {
			return port, stop, err
		}
	}
	return 0, nil, &PortTakenError{Host: host, First: first, Last: last}
}

const (
	forwardReady  = "cc-forward-ready"
	forwardFailed = "remote port forwarding failed"
	// forwardScript prints its marker once the forward is up and holds the connection until its stdin closes.
	forwardScript = "echo " + forwardReady + "; exec cat >/dev/null"
)

// sshConn is a running ssh: its stdout, its stderr once it has exited, and stop, which ends it.
type sshConn struct {
	out    *bufio.Reader
	stderr func() string
	stop   func()
}

// startSSH runs argv with a stdin it holds open and stdout it reads; tests swap it.
var startSSH = func(ctx context.Context, argv []string) (*sshConn, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	var errs bytes.Buffer
	cmd.Stdout, cmd.Stderr, cmd.WaitDelay = pw, &errs, 2*time.Second
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	finished := make(chan struct{})
	go func() { _ = cmd.Wait(); _ = pw.Close(); close(finished) }()
	return &sshConn{
		out:    bufio.NewReader(pr),
		stderr: func() string { <-finished; return errs.String() },
		stop: sync.OnceFunc(func() {
			_ = stdin.Close()
			_ = cmd.Process.Kill()
			<-finished
		}),
	}, nil
}

// forwardOnce opens one -R forward on its own ssh connection and returns once it is up; stop closes it.
func forwardOnce(ctx context.Context, host string, port, local int) (func(), error) {
	argv, err := sshArgv(host, false, true, forwardScript)
	if err != nil {
		return nil, err
	}
	argv = withTunnel(slices.Insert(argv, 1, "-o", "ConnectTimeout=10"), "-R", fmt.Sprintf("%d:localhost:%d", port, local))
	conn, err := startSSH(ctx, argv)
	if err != nil {
		return nil, fmt.Errorf("ssh to %s: %w", host, err)
	}
	if line, _ := conn.out.ReadString('\n'); strings.TrimSpace(line) == forwardReady {
		return conn.stop, nil
	}
	conn.stop()
	msg := conn.stderr()
	if strings.Contains(msg, forwardFailed) {
		return nil, &PortTakenError{Host: host, First: port, Last: port}
	}
	if ctx.Err() != nil {
		return nil, fmt.Errorf("ssh to %s: %w", host, ctx.Err())
	}
	return nil, fmt.Errorf("ssh to %s: %s", host, firstLine(msg))
}

// firstLine is the first non-empty line of s, clipped, for a one-line error.
func firstLine(s string) string {
	for l := range strings.SplitSeq(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return textutil.CapRunes(l, 160, "…")
		}
	}
	return "connection closed"
}

// OpenURLOn asks host to open url in its browser: `claude-code databricks open-url <url>` over ssh, url one argument.
func OpenURLOn(ctx context.Context, host, url string) error {
	argv, err := hostArgv(host, []string{"databricks", "open-url", url}, false)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var errs bytes.Buffer
	cmd := exec.CommandContext(ctx, argv[0], slices.Insert(argv[1:], 0, "-o", "BatchMode=yes")...)
	cmd.Stderr = &errs
	if err := runCommand(cmd); err != nil {
		return fmt.Errorf("open the URL on %s: %w: %s", host, err, firstLine(errs.String()))
	}
	return nil
}
