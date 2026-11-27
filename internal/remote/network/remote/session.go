package remote

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/platform/proc"
	"github.com/edotau/claude-code/internal/platform/textutil"
)

// sshTimeout bounds one ssh round trip (start, list or pane capture); attach replaces the process and has no deadline.
const sshTimeout = 20 * time.Second

// execArgs runs script under sh locally when local (host is this machine), else over ssh.
func execArgs(host, script string, local bool) ([]string, error) {
	if local {
		return []string{"sh", "-c", script}, nil
	}
	return sshArgv(host, false, false, script)
}

// attachArgs is the argv attaching to a tmux session on host; it replaces the caller's process so tmux owns the TTY.
func attachArgs(host, session string, local bool) ([]string, error) {
	if local {
		return []string{"tmux", "attach", "-t", session}, nil
	}
	return sshArgv(host, true, false, "tmux attach -t "+textutil.ShellQuote(session))
}

// attachCommand is the attach hint printed after a dispatch: attachArgs in the readable form a person types.
func attachCommand(host, session string, local bool) string {
	if local {
		return "tmux attach -t " + session
	}
	return "ssh -t -- " + host + " tmux attach -t " + session
}

// sshCommand builds argv as a command bounded by sshTimeout; the caller picks the output mode.
func sshCommand(ctx context.Context, argv []string) (*exec.Cmd, context.CancelFunc) {
	cctx, cancel := context.WithTimeout(ctx, sshTimeout)
	cmd := exec.CommandContext(cctx, argv[0], argv[1:]...)
	cmd.WaitDelay = proc.ExecWaitDelay
	return cmd, cancel
}

// runAgent runs an agent start argv over ssh under sshTimeout and returns the combined output even on error.
func runAgent(ctx context.Context, argv []string) ([]byte, error) {
	cmd, cancel := sshCommand(ctx, argv)
	defer cancel()
	return proc.CombinedOutputCapped(cmd)
}

// listSessions lists the cc-* tmux sessions on host.
func listSessions(ctx context.Context, host string, local bool) ([]Session, error) {
	argv, err := execArgs(host, listCommand, local)
	if err != nil {
		return nil, err
	}
	out, err := execRunner(ctx, argv)
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(ee.Stderr) > 0 {
		return nil, fmt.Errorf("list tmux sessions on %s: %s: %w", host, strings.TrimSpace(string(ee.Stderr)), err)
	}
	if err != nil {
		return nil, fmt.Errorf("list tmux sessions on %s: %w", host, err)
	}
	return parseSessions(string(out)), nil
}

// pollPaneStatus samples pane status once (interval <= 0) or repeatedly until Ctrl-C, calling emit per sample.
func pollPaneStatus(
	ctx context.Context, host string, local bool, interval time.Duration, emit func([]paneStatus) error,
) error {
	sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	var prev []paneStatus
	for {
		caps, capErr := capturePanes(sigCtx, host, local, execRunner)
		prev = observePanes(prev, caps, capErr, time.Now())
		if capErr != nil && len(prev) == 0 {
			return capErr
		}
		if err := emit(prev); err != nil {
			return err
		}
		if interval <= 0 {
			return nil
		}
		select {
		case <-sigCtx.Done():
			return nil
		case <-time.After(interval):
		}
	}
}

// Seams: tests swap the reconnect clock and the wait between attempts.
var (
	attachNow  = time.Now
	attachWait = func(ctx context.Context, d time.Duration) {
		select {
		case <-ctx.Done():
		case <-time.After(d):
		}
	}
)

// attachLadder is the wait before each reconnect; a run that lasts attachStable restarts it.
var attachLadder = []time.Duration{1, 2, 5, 5, 10, 10, 10, 30, 30}

const (
	attachStable = 60 * time.Second
	sshLostCode  = 255 // ssh's own exit status for a failed or lost connection
)

// busyLocalPort is the first of ports that cannot be listened on at local loopback, else 0.
func busyLocalPort(ctx context.Context, ports []int) int {
	var lc net.ListenConfig
	for _, p := range ports {
		l, err := lc.Listen(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)))
		if err != nil {
			return p
		}
		_ = l.Close()
	}
	return 0
}

// attachSession attaches to a tmux session on host: a local one replaces this process, a remote one reconnects.
func attachSession(host, session string, local bool, ports []int) error {
	session, err := normalizeName(session)
	if err != nil {
		return err
	}
	argv, err := attachArgs(host, session, local)
	if err != nil {
		return err
	}
	if !local {
		return attachReconnecting(host, argv, ports)
	}
	if len(ports) > 0 {
		return errors.New("--forward needs a remote host")
	}
	bin, err := exec.LookPath(argv[0])
	if err != nil {
		return err
	}
	return proc.Exec(bin, argv[1:], os.Environ())
}

// attachReconnecting runs the ssh attach until it ends for a reason other than a lost connection (exit 255).
func attachReconnecting(host string, argv []string, ports []int) error {
	for _, p := range ports {
		argv = withForward(argv, p)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	fails := 0
	for {
		if p := busyLocalPort(ctx, ports); p != 0 {
			return fmt.Errorf("local port %d is busy; cannot forward it to %s", p, host)
		}
		cmd := exec.CommandContext(context.Background(), argv[0], argv[1:]...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		start := attachNow()
		err := runCommand(cmd)
		if exit, ok := errors.AsType[*exec.ExitError](err); err == nil || !ok || exit.ExitCode() != sshLostCode {
			return err
		}
		if attachNow().Sub(start) >= attachStable {
			fails = 0
		}
		if fails++; fails > len(attachLadder) {
			return fmt.Errorf("gave up on %s after %d failed reconnects", host, len(attachLadder))
		}
		wait := attachLadder[fails-1] * time.Second
		fmt.Fprintf(os.Stderr, "connection to %s lost; reconnecting in %s (attempt %d/%d), Ctrl-C to stop\n",
			host, wait, fails, len(attachLadder))
		if attachWait(ctx, wait); ctx.Err() != nil {
			return nil
		}
	}
}
