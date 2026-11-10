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

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/vault"
)

// errNoSubnetRoutes means this box advertises no subnet routes to approve.
var errNoSubnetRoutes = errors.New("this box advertises no subnet routes; run --subnet <cidr> first")

// resolveAuthKey picks the auth key: literal ("-" reads stdin), then the keyEnv variable, then the vault.
func resolveAuthKey(authKey, keyEnv string, stdin io.Reader) (string, error) {
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

// connected reports the local status when the backend is already Running.
func connected(ctx context.Context) (Local, bool) {
	st, err := Status(ctx)
	return st, err == nil && st.BackendState == "Running"
}

// approveSelfExitNode approves this box's exit-node routes and returns its DNS name.
func approveSelfExitNode(ctx context.Context) (string, error) {
	client, st, id, err := selfDevice(ctx)
	if err != nil {
		return "", err
	}
	if err := client.ApproveExitNode(ctx, id); err != nil {
		return st.DNSName, err
	}
	return st.DNSName, nil
}

// approveSelfSubnets enables every subnet route this box advertises, keeping routes already enabled.
func approveSelfSubnets(ctx context.Context) (dns string, want, enabled []string, err error) {
	client, st, id, err := selfDevice(ctx)
	if err != nil {
		return "", nil, nil, err
	}
	cur, err := client.Routes(ctx, id)
	if err != nil {
		return st.DNSName, nil, nil, err
	}
	want = subnetRoutes(cur.Advertised)
	if len(want) == 0 {
		return st.DNSName, nil, nil, errNoSubnetRoutes
	}
	en, err := client.EnableRoutes(ctx, id, cur, want)
	if err != nil {
		return st.DNSName, want, nil, err
	}
	return st.DNSName, want, subnetRoutes(en), nil
}

// parseSubnets splits a comma-separated CIDR list; ok is false when any entry is not a prefix.
func parseSubnets(value string) (routes []string, ok bool) {
	routes = strings.Split(value, ",")
	for i := range routes {
		routes[i] = strings.TrimSpace(routes[i])
	}
	bad := slices.ContainsFunc(routes, func(r string) bool { _, err := netip.ParsePrefix(r); return err != nil })
	return routes, !bad
}

// mintKey parses expiry, mints an auth key through the admin API and hints at the usual 404 cause.
func mintKey(ctx context.Context, expiry string, opts keyOpts) (AuthKey, error) {
	exp, err := parseKeyExpiry(expiry)
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

// connectAudited joins the tailnet with key and audits the connect under this node's name.
func connectAudited(ctx context.Context, key string, advertise bool) error {
	if err := connect(ctx, key, advertise); err != nil {
		return err
	}
	auditConnect(ctx)
	return nil
}

// auditConnect logs the connect with the node name, or notes that status was unavailable.
func auditConnect(ctx context.Context) {
	st, err := Status(ctx)
	if err != nil {
		audit.Log(audit.TailscaleConnect, "", "connect (status unavailable: "+err.Error()+")")
		return
	}
	audit.Log(audit.TailscaleConnect, st.DNSName, "connect")
}

// advertiseConnected advertises the already-connected node dns as an exit node and audits it.
func advertiseConnected(ctx context.Context, dns string) error {
	if err := advertiseExitNode(ctx, true); err != nil {
		return err
	}
	audit.Log(audit.TailscaleExitNode, dns, "enable")
	return nil
}

// mintKeyAudited mints an auth key (MintKey) and audits the mint with its tags.
func mintKeyAudited(ctx context.Context, expiry string, opts keyOpts) (AuthKey, error) {
	key, err := mintKey(ctx, expiry, opts)
	if err != nil {
		return AuthKey{}, err
	}
	audit.Log(audit.TailscaleKeyMint, "tailnet", "mint:"+strings.Join(opts.Tags, ","))
	return key, nil
}

// saveAuthKey seals key as TAILSCALE_AUTHKEY, the vault entry ResolveAuthKey falls back to.
func saveAuthKey(key string) error { return vault.Store("TAILSCALE_AUTHKEY", key) }
