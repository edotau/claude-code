package tailscale

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/vault"
)

// ErrNoSubnetRoutes means this box advertises no subnet routes to approve.
var ErrNoSubnetRoutes = errors.New("this box advertises no subnet routes; run --subnet <cidr> first")

// SSHTimeout bounds one ssh round trip (start or list); attach replaces the process and has no deadline.
const SSHTimeout = 20 * time.Second

// ResolveAuthKey picks the auth key: literal ("-" reads stdin), then the keyEnv variable, then the vault.
func ResolveAuthKey(authKey, keyEnv string, stdin io.Reader) (string, error) {
	key := authKey
	if key == "-" {
		in, err := io.ReadAll(stdin)
		if err != nil {
			return "", err
		}
		key = strings.TrimSpace(string(in))
	}
	if key == "" && keyEnv != "" {
		key = strings.TrimSpace(os.Getenv(keyEnv))
	}
	if key == "" {
		key, _, _ = vault.Get("TAILSCALE_AUTHKEY")
	}
	return key, nil
}

// Connected reports the local status when the backend is already Running.
func Connected(ctx context.Context) (Local, bool) {
	st, err := Status(ctx)
	return st, err == nil && st.BackendState == "Running"
}

// ApproveSelfExitNode approves this box's exit-node routes and returns its DNS name.
func ApproveSelfExitNode(ctx context.Context) (string, error) {
	client, st, id, err := selfDevice(ctx)
	if err != nil {
		return "", err
	}
	if err := client.ApproveExitNode(ctx, id); err != nil {
		return st.DNSName, err
	}
	return st.DNSName, nil
}

// ApproveSelfSubnets enables every subnet route this box advertises, keeping routes already enabled.
func ApproveSelfSubnets(ctx context.Context) (dns string, want, enabled []string, err error) {
	client, st, id, err := selfDevice(ctx)
	if err != nil {
		return "", nil, nil, err
	}
	cur, err := client.Routes(ctx, id)
	if err != nil {
		return st.DNSName, nil, nil, err
	}
	want = SubnetRoutes(cur.Advertised)
	if len(want) == 0 {
		return st.DNSName, nil, nil, ErrNoSubnetRoutes
	}
	en, err := client.EnableRoutes(ctx, id, cur, want)
	if err != nil {
		return st.DNSName, want, nil, err
	}
	return st.DNSName, want, SubnetRoutes(en), nil
}

// ParseSubnets splits a comma-separated CIDR list; ok is false when any entry is not a prefix.
func ParseSubnets(value string) (routes []string, ok bool) {
	routes = strings.Split(value, ",")
	bad := slices.ContainsFunc(routes, func(r string) bool { _, err := netip.ParsePrefix(r); return err != nil })
	return routes, !bad
}

// execWaitDelay force-closes a timed-out command's pipes so surviving children cannot hold Wait open.
const execWaitDelay = 2 * time.Second

// SSHCommand builds argv as a command bounded by SSHTimeout; the caller picks the output mode.
func SSHCommand(ctx context.Context, argv []string) (*exec.Cmd, context.CancelFunc) {
	cctx, cancel := context.WithTimeout(ctx, SSHTimeout)
	cmd := exec.CommandContext(cctx, argv[0], argv[1:]...)
	cmd.WaitDelay = execWaitDelay
	return cmd, cancel
}

// RunRemote runs argv over ssh under SSHTimeout and returns the combined output even on error.
func RunRemote(ctx context.Context, argv []string) ([]byte, error) {
	cmd, cancel := SSHCommand(ctx, argv)
	defer cancel()
	return cmd.CombinedOutput()
}

// ListRemoteSessions lists the cc-* tmux sessions on host.
func ListRemoteSessions(ctx context.Context, host string) ([]RemoteSession, error) {
	cmd, cancel := SSHCommand(ctx, ExecArgs(ctx, host, ListCommand(), false))
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
func PollPaneStatus(ctx context.Context, host string, interval time.Duration, emit func([]PaneStatus) error) error {
	sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	var prev []PaneStatus
	for {
		caps, capErr := CapturePanes(sigCtx, host, false, ExecRunner)
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
func AttachSession(host, session string) error {
	if !ValidName(session) {
		return fmt.Errorf("session %q must match ^[A-Za-z0-9._-]{1,64}$", session)
	}
	argv := AttachArgs(host, session)
	bin, err := exec.LookPath(argv[0])
	if err != nil {
		return err
	}
	return proc.Exec(bin, argv[1:], os.Environ())
}

// MachinesCached lists tailnet machines and refreshes the `dispatch --host` completion cache.
func MachinesCached(ctx context.Context) ([]Machine, error) {
	machines, err := Machines(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(machines))
	for _, m := range machines {
		names = append(names, m.Name)
	}
	WriteMachinesCache(names)
	return machines, nil
}

// MintKey parses expiry, mints an auth key through the admin API and hints at the usual 404 cause.
func MintKey(ctx context.Context, expiry string, opts KeyOpts) (AuthKey, error) {
	exp, err := ParseKeyExpiry(expiry)
	if err != nil {
		return AuthKey{}, fmt.Errorf("--expiry: %w", err)
	}
	client, err := NewClient()
	if err != nil {
		return AuthKey{}, err
	}
	opts.Expiry = exp
	key, err := client.CreateAuthKey(ctx, opts)
	if err != nil && strings.Contains(err.Error(), "HTTP 404") {
		// The admin API answers 404, not 403, when an OAuth client lacks the auth_keys scope.
		return AuthKey{}, fmt.Errorf("%w\nhint: the OAuth client likely lacks Keys → Auth Keys: write, "+
			"and OAuth-minted keys need --tags tag:<x> (a tag in tagOwners); or use TAILSCALE_API_KEY", err)
	}
	return key, err
}
