package remote

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/textutil"
)

// sshTimeout bounds one ssh round trip (start or list); attach replaces the process and has no deadline.
const sshTimeout = 20 * time.Second

// execWaitDelay force-closes a timed-out command's pipes so surviving children cannot hold Wait open.
const execWaitDelay = 2 * time.Second

// ExecArgs runs script under sh locally when local (host is this machine), else over ssh; batch forbids ssh prompts.
func ExecArgs(host, script string, local, batch bool) ([]string, error) {
	if local {
		return []string{"sh", "-c", script}, nil
	}
	return Argv(host, false, batch, script)
}

// AttachArgs is the argv attaching to a tmux session on host; it replaces the caller's process so tmux owns the TTY.
func AttachArgs(host, session string, local bool) ([]string, error) {
	if local {
		return []string{"tmux", "attach", "-t", session}, nil
	}
	return Argv(host, true, false, "tmux attach -t "+textutil.ShellQuote(session))
}

// AttachCommand is the attach hint printed after a dispatch: AttachArgs in the readable form a person types.
func AttachCommand(host, session string, local bool) string {
	if local {
		return "tmux attach -t " + session
	}
	return "ssh -t -- " + host + " tmux attach -t " + session
}

// sshCommand builds argv as a command bounded by sshTimeout; the caller picks the output mode.
func sshCommand(ctx context.Context, argv []string) (*exec.Cmd, context.CancelFunc) {
	cctx, cancel := context.WithTimeout(ctx, sshTimeout)
	cmd := exec.CommandContext(cctx, argv[0], argv[1:]...)
	cmd.WaitDelay = execWaitDelay
	return cmd, cancel
}

// RunRemote runs argv over ssh under sshTimeout and returns the combined output even on error.
func RunRemote(ctx context.Context, argv []string) ([]byte, error) {
	cmd, cancel := sshCommand(ctx, argv)
	defer cancel()
	return cmd.CombinedOutput()
}

// ListRemoteSessions lists the cc-* tmux sessions on host.
func ListRemoteSessions(ctx context.Context, host string, local bool) ([]RemoteSession, error) {
	argv, err := ExecArgs(host, listCommand, local, false)
	if err != nil {
		return nil, err
	}
	cmd, cancel := sshCommand(ctx, argv)
	defer cancel()
	out, err := cmd.Output()
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(ee.Stderr) > 0 {
		return nil, fmt.Errorf("list tmux sessions on %s: %s: %w", host, strings.TrimSpace(string(ee.Stderr)), err)
	}
	if err != nil {
		return nil, err
	}
	return ParseSessions(string(out)), nil
}

// PollPaneStatus samples pane status once (interval <= 0) or repeatedly until Ctrl-C, calling emit per sample.
func PollPaneStatus(ctx context.Context, host string, local bool, interval time.Duration, emit func([]PaneStatus) error) error {
	sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	var prev []PaneStatus
	for {
		caps, capErr := CapturePanes(sigCtx, host, local, false, ExecRunner)
		prev = ObservePanes(prev, caps, capErr, time.Now())
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

// AttachSession replaces this process with an ssh attach to a tmux session on host.
func AttachSession(host, session string, local bool) error {
	if !ValidName(session) {
		return fmt.Errorf("session %q must match ^[A-Za-z0-9._-]{1,64}$", session)
	}
	argv, err := AttachArgs(host, session, local)
	if err != nil {
		return err
	}
	bin, err := exec.LookPath(argv[0])
	if err != nil {
		return err
	}
	return proc.Exec(bin, argv[1:], os.Environ())
}
