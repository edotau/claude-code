package tailscale

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/vault"
)

// ErrNoSubnetRoutes means this box advertises no subnet routes to approve.
var ErrNoSubnetRoutes = errors.New("this box advertises no subnet routes; run --subnet <cidr> first")

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
