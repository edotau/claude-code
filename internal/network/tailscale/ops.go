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
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/vault"
)

// errNoSubnetRoutes means this box advertises no subnet routes to approve.
var errNoSubnetRoutes = errors.New("this box advertises no subnet routes; run --subnet <cidr> first")

// errStdinEmpty: --auth-key - read no key from stdin; falling back to another key source would hide the mistake.
var errStdinEmpty = errors.New("--auth-key -: stdin is empty")

// errKeyEnvEmpty: --key-env named a variable that holds no key; the vault key must not stand in for it.
var errKeyEnvEmpty = errors.New("is empty or unset")

// resolveAuthKey picks the auth key: literal ("-" reads stdin), then the keyEnv variable, then the vault.
func resolveAuthKey(authKey, keyEnv string, stdin io.Reader) (string, error) {
	key := authKey
	if key == "-" {
		in, err := io.ReadAll(stdin)
		if err != nil {
			return "", fmt.Errorf("reading --auth-key from stdin: %w", err)
		}
		if key = strings.TrimSpace(string(in)); key == "" {
			return "", errStdinEmpty
		}
	}
	if key == "" && keyEnv != "" {
		if key = strings.TrimSpace(os.Getenv(keyEnv)); key == "" {
			return "", fmt.Errorf("--key-env %s %w; export it or drop --key-env", keyEnv, errKeyEnvEmpty)
		}
	}
	if key == "" {
		key, _, _ = vault.Get(authKeyName)
	}
	return key, nil
}

// connected reports the local status when the backend is already Running.
func connected(ctx context.Context) (Local, bool) {
	st, err := Status(ctx)
	return st, err == nil && st.running()
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

// KeyOptions are a minted auth key's settings: Expiry is a Go duration or Nd, Tags a CSV of ACL tags.
type KeyOptions struct {
	Expiry, Tags, Description          string
	Reusable, Ephemeral, Preauthorized bool
}

// MintKey parses the expiry, mints an auth key through the admin API and audits the mint with its tags.
func MintKey(ctx context.Context, o KeyOptions) (AuthKey, error) {
	exp, err := parseKeyExpiry(o.Expiry)
	if err != nil {
		return AuthKey{}, fmt.Errorf("--expiry: %w", err)
	}
	client, err := NewClient()
	if err != nil {
		return AuthKey{}, err
	}
	opts := keyOpts{Ephemeral: o.Ephemeral, Preauthorized: o.Preauthorized, Reusable: o.Reusable,
		Tags: textutil.SplitCSV(o.Tags), Description: o.Description, Expiry: exp}
	key, err := client.CreateAuthKey(ctx, opts)
	if err != nil && strings.Contains(err.Error(), "HTTP 404") {
		// The admin API answers 404, not 403, when an OAuth client lacks the auth_keys scope.
		return AuthKey{}, fmt.Errorf("%w\nhint: the OAuth client likely lacks Keys → Auth Keys: write, "+
			"and OAuth-minted keys need --tags tag:<x> (a tag in tagOwners); or use TAILSCALE_API_KEY", err)
	}
	if err != nil {
		return AuthKey{}, err
	}
	audit.Log(audit.TailscaleKeyMint, "tailnet", "mint:"+strings.Join(opts.Tags, ","))
	return key, nil
}

// SaveAuthKey seals key as TAILSCALE_AUTHKEY, the vault entry resolveAuthKey falls back to.
func SaveAuthKey(key string) error { return vault.Store(authKeyName, key) }

// ExitNode advertises (on), stops (off) or approves (approve) this box as an exit node; it returns the outcome line.
func ExitNode(ctx context.Context, mode string) (string, error) {
	switch mode {
	case "on", "off":
		if err := advertiseExitNode(ctx, mode == "on"); err != nil {
			return "", err
		}
		if mode == "on" {
			audit.Log(audit.TailscaleExitNode, "self", "enable")
			return "tailscale: advertising as an exit node", nil
		}
		audit.Log(audit.TailscaleExitNode, "self", "disable")
		return "tailscale: no longer advertising as an exit node", nil
	case "approve":
		dns, err := approveSelfExitNode(ctx)
		if err != nil {
			return "", err
		}
		audit.Log(audit.TailscaleExitNode, dns, "approve")
		return "tailscale: exit-node routes approved", nil
	}
	return "", fmt.Errorf("exit node mode must be on, off or approve, got %q", mode)
}

// Cred is one Tailscale credential name/value pair waiting to be stored together.
type Cred struct{ Name, Value string }

// CheckCreds refuses an empty or multi-line value so a pair is never half-written.
func CheckCreds(creds []Cred) error {
	for _, c := range creds {
		if c.Value == "" {
			return fmt.Errorf("empty value for %s", c.Name)
		}
		if strings.ContainsAny(c.Value, "\r\n") { // vault.Store rejects these
			return fmt.Errorf("%s spans lines", c.Name)
		}
	}
	return nil
}

// DryVerb is what a dry-run write of c would do: "replace" when the vault already has the name, else "add".
func DryVerb(c Cred) string {
	if slices.Contains(vault.Names(), c.Name) {
		return "replace"
	}
	return "add"
}

// priorCred is a vault value a StoreCreds write is about to overwrite, kept so it can be restored.
type priorCred struct {
	name, value string
	had         bool
}

// StoreCreds writes every pair or none: a failed write restores the earlier pairs' previous values.
func StoreCreds(creds []Cred) error {
	var done []priorCred
	for _, c := range creds {
		old, had, err := vault.ReadSecret(c.Name)
		if err != nil {
			return errors.Join(fmt.Errorf("read %s: %w", c.Name, err), restoreCreds(done))
		}
		if err := vault.Store(c.Name, c.Value); err != nil {
			return errors.Join(err, restoreCreds(done))
		}
		done = append(done, priorCred{name: c.Name, value: old, had: had})
	}
	return nil
}

// restoreCreds puts each prior value back, newest first; a name that was absent is deleted again.
func restoreCreds(done []priorCred) error {
	var errs []error
	for _, p := range slices.Backward(done) {
		if p.had {
			errs = append(errs, vault.Store(p.name, p.value))
		} else {
			errs = append(errs, vault.Delete(p.name))
		}
	}
	return errors.Join(errs...)
}
