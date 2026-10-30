package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/databricks/oauth"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/vault"
)

// ErrNeedsLogin matches (errors.Is) a u2m credential that only a fresh browser login can restore.
var ErrNeedsLogin = errors.New("login required")

// LoginRequiredError is a u2m row with no sealed refresh token or one the token endpoint rejected.
type LoginRequiredError struct {
	Provider string
	err      error
}

func (e *LoginRequiredError) Error() string { return e.err.Error() }
func (e *LoginRequiredError) Unwrap() error { return e.err }

// Is makes errors.Is(err, ErrNeedsLogin) true.
func (e *LoginRequiredError) Is(target error) bool { return target == ErrNeedsLogin }

var secretIDRE = regexp.MustCompile(`[^A-Z0-9]+`)

// secretSegment upper-cases s and turns every run of other characters into one underscore.
func secretSegment(s string) string {
	return strings.Trim(secretIDRE.ReplaceAllString(strings.ToUpper(s), "_"), "_")
}

// SecretName is the vault name <PROVIDER>_<PROFILE>_<AUTH> of a profile's sealed secret (OAUTH, M2M, PAT, KEY);
// the profile segment is written once when it equals the provider (ANTHROPIC_KEY). "" for a method with none.
func SecretName(provider, profile, method string) string {
	switch method {
	case AuthOAuth, AuthM2M, AuthPAT, AuthKey:
	default:
		return ""
	}
	prov, prof := secretSegment(provider), secretSegment(profile)
	if prov == "" || prof == "" {
		return ""
	}
	if prov == prof {
		return prov + "_" + strings.ToUpper(method)
	}
	return prov + "_" + prof + "_" + strings.ToUpper(method)
}

// AuthSecretName is the vault name of p's sealed secret for its sealed method; "" for rows with none.
func AuthSecretName(p *Provider) string {
	if !sealedAuth(p.Auth.Type) {
		return ""
	}
	return SecretName(p.provider(), p.Name, p.Auth.Method())
}

// KeySecretName is the vault name of a key row's static key; "" for any other row.
func KeySecretName(p *Provider) string {
	if p.Auth.Method() != AuthKey {
		return ""
	}
	return SecretName(p.provider(), p.Name, AuthKey)
}

// oauthValue is an oauth profile's one sealed value: the refresh and the cached access token.
type oauthValue struct {
	Refresh string `json:"refresh,omitempty"`
	Access  string `json:"access,omitempty"`
}

// readOAuth reads p's sealed {refresh, access}.
func readOAuth(p *Provider) (oauthValue, error) {
	var ov oauthValue
	raw, found, err := vault.ReadSecret(AuthSecretName(p))
	if err != nil {
		return ov, err
	}
	if !found {
		return oauthValue{}, nil
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &ov); err != nil {
		return oauthValue{}, fmt.Errorf("providers: sealed oauth value %q is not valid JSON: %w", AuthSecretName(p), err)
	}
	return ov, nil
}

// writeOAuth seals ov as the one JSON value under p's oauth secret name.
func writeOAuth(p *Provider, ov oauthValue) error {
	b, err := json.Marshal(ov)
	if err != nil {
		return err
	}
	return SealSecret(AuthSecretName(p), string(b))
}

// sealedAuth reports whether the auth type keeps its credential in the vault.
func sealedAuth(t string) bool { return slices.Contains(supportedAuthTypes, t) }

// AuthHost is the https://host a row talks to: the preset host, else BaseURL's origin; "" when neither parses.
func AuthHost(p *Provider) string {
	h := rawAuthHost(p)
	if u, err := url.Parse(h); err == nil && u.Scheme == "http" && !loopbackHost(u.Host) {
		return ""
	}
	return h
}

// rawAuthHost is AuthHost before the scheme check.
func rawAuthHost(p *Provider) string {
	if h := presetHost(p.Host); h != "" {
		return h
	}
	u, err := url.Parse(p.BaseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// loopbackHost reports whether h (host or host:port) is a loopback name, the only plain-http exception.
func loopbackHost(h string) bool {
	name := h
	if u, err := url.Parse("//" + h); err == nil {
		name = u.Hostname()
	}
	return name == "localhost" || name == "127.0.0.1" || name == "::1"
}

// checkAuthScheme refuses a sealed auth type whose host is plain http outside loopback; the error carries no credential.
func checkAuthScheme(p *Provider) error {
	u, err := url.Parse(rawAuthHost(p))
	if err != nil || u.Scheme != "http" || loopbackHost(u.Host) {
		return nil
	}
	return fmt.Errorf("%s: auth.type %s needs an https host, got scheme %q", p.Name, p.Auth.Type, u.Scheme)
}

// supportedAuthTypes are the auth types that keep their credential in the vault.
var supportedAuthTypes = []string{AuthOAuth, AuthM2M, AuthPAT}

var (
	accountIDRE   = regexp.MustCompile(`^[0-9a-fA-F-]{8,64}$`)
	workspaceIDRE = regexp.MustCompile(`^[0-9]+$`)
)

// validateAuthType checks a sealed row's companion fields and the account/workspace ids for one provider.
func validateAuthType(n string, p *Provider) []string {
	var errs []string
	if id := p.Auth.AccountID; id != "" && !accountIDRE.MatchString(id) {
		errs = append(errs, fmt.Sprintf("%s: auth.accountId %q must match [0-9a-fA-F-]{8,64}", n, id))
	}
	if id := p.Auth.WorkspaceID; id != "" && !workspaceIDRE.MatchString(id) {
		errs = append(errs, fmt.Sprintf("%s: auth.workspaceId %q must be digits only", n, id))
	}
	k := p.Auth.Type
	if sealedAuth(k) {
		if err := checkAuthScheme(p); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if k == AuthM2M && p.Auth.ClientID == "" {
		errs = append(errs, n+": auth.type "+AuthM2M+" needs auth.clientId")
	}
	if strings.ContainsAny(p.Auth.ClientID, "\r\n[=") {
		errs = append(errs, n+": auth.clientId must not contain CR, LF, '[' or '='")
	}
	if k != AuthM2M && p.Auth.ClientID != "" {
		errs = append(errs, n+": auth.clientId needs auth.type "+AuthM2M)
	}
	return errs
}

// refreshLockWait bounds how long a refresh waits for another process holding the provider's refresh lock.
const refreshLockWait = 60 * time.Second

var lockNameRE = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// withRefreshLock serializes a provider's refresh-token use across processes; mintOnce covers goroutines.
func withRefreshLock(p *Provider, fn func() error) error {
	dir := filepath.Join(paths.StateDir(), "locks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("%s: refresh lock: %w", p.Name, err)
	}
	lock := filepath.Join(dir, "auth-"+lockNameRE.ReplaceAllString(p.Name, "_")+".lock")
	ran, err := paths.TryFileLock(lock, refreshLockWait, fn)
	if err != nil {
		return err
	}
	if !ran {
		return fmt.Errorf("%s: timed out waiting for another refresh to finish", p.Name)
	}
	return nil
}

// authCredential mints (or serves from cache) the access token for a sealed auth type; force skips the cache.
func authCredential(ctx context.Context, p *Provider, force bool) (string, error) {
	if p.Auth.Type != AuthOAuth {
		return mintOnce(ctx, p, force, func(ctx context.Context) (string, error) { return mintAndCache(ctx, p) })
	}
	return mintOnce(ctx, p, force, func(ctx context.Context) (string, error) {
		var out string
		// mintAuth re-reads the sealed refresh token, so a rotation by another process is picked up.
		err := withRefreshLock(p, func() error {
			if !force {
				if v, ok := sealedAccess(p); ok {
					out = v
					return finishCredential(p, v, time.Time{})
				}
			}
			v, err := mintAndCache(ctx, p)
			out = v
			if err == nil {
				sealAccess(p, v)
			}
			return err
		})
		return out, err
	})
}

// sealedAccess returns the sealed access token when it stays valid past the expiry margin.
func sealedAccess(p *Provider) (string, bool) {
	ov, err := readOAuth(p)
	v := strings.TrimSpace(ov.Access)
	if err != nil || v == "" {
		return "", false
	}
	exp, ok := jwtExp(v)
	if !ok || !time.Unix(exp, 0).Add(-expiryMargin).After(time.Now()) {
		return "", false
	}
	return v, true
}

// sealAccess caches a u2m access token beside the refresh token; a seal error is logged by name only.
func sealAccess(p *Provider, tok string) {
	ov, err := readOAuth(p)
	if err != nil { // a failed read must not re-seal {access} alone and drop the refresh token
		fmt.Fprintf(os.Stderr, "%s: cache access token (%s) skipped: vault read failed\n", p.Name, AuthSecretName(p))
		return
	}
	ov.Access = tok
	if err := writeOAuth(p, ov); err != nil {
		fmt.Fprintf(os.Stderr, "%s: cache access token (%s) failed\n", p.Name, AuthSecretName(p))
	}
}

func mintAndCache(ctx context.Context, p *Provider) (string, error) {
	tok, err := mintAuth(ctx, p)
	if err != nil {
		return "", err
	}
	if err := finishCredential(p, tok.Access, tok.Expiry); err != nil {
		return "", err
	}
	return tok.Access, nil
}

// mintAuth is the one path that turns a sealed credential into an access token.
func mintAuth(ctx context.Context, p *Provider) (oauth.Token, error) {
	name := AuthSecretName(p)
	secret, err := mintSecret(p)
	if err != nil {
		return oauth.Token{}, fmt.Errorf("%s: vault: %w", p.Name, err)
	}
	if secret == "" {
		err := fmt.Errorf("%s: no credential (%s)", p.Name, authHint(p, name))
		if p.Auth.Type == AuthOAuth {
			err = &LoginRequiredError{Provider: p.Name, err: err}
		}
		return oauth.Token{}, err
	}
	if err := checkAuthScheme(p); err != nil {
		return oauth.Token{}, err
	}
	host := AuthHost(p)
	switch p.Auth.Type {
	case AuthPAT:
		return oauth.Token{Access: secret}, nil
	case AuthM2M:
		return oauth.MintM2M(ctx, host, p.Auth.ClientID, secret)
	}
	tok, err := oauth.RefreshU2M(ctx, host, secret)
	if err != nil {
		wrapped := fmt.Errorf("%s: %w (run `claude-code databricks login --profile %s`)", p.Name, err, p.Name)
		if oauth.IsLoginRequired(err) {
			wrapped = &LoginRequiredError{Provider: p.Name, err: wrapped}
		}
		return oauth.Token{}, wrapped
	}
	if tok.Refresh != "" && tok.Refresh != secret {
		if err := writeOAuth(p, oauthValue{Refresh: tok.Refresh, Access: tok.Access}); err != nil {
			return oauth.Token{}, fmt.Errorf("%s: re-seal rotated refresh token: %w", p.Name, err)
		}
	}
	return tok, nil
}

// mintSecret reads the sealed secret; u2m reads the vault only so a stale env value never replays a rotated token.
func mintSecret(p *Provider) (string, error) {
	if p.Auth.Type == AuthOAuth {
		ov, err := readOAuth(p)
		return strings.TrimSpace(ov.Refresh), err
	}
	v, _, err := EffectiveSecret(AuthSecretName(p))
	return v, err
}

func authHint(p *Provider, name string) string {
	if p.Auth.Type == AuthOAuth {
		return "run `claude-code databricks login --profile " + p.Name + "`, or `claude-code configure` and pick Set up"
	}
	return "set " + name + " with `claude-code vault set`"
}

// sealer is the registered one-line sealer; internal/secrets installs Store (metadata prefix + audit) at init.
var sealer = sealDirect

// RegisterSealer routes SealSecret through fn so every sealed write lands the same record and audit entry.
func RegisterSealer(fn func(name, value string) error) { sealer = fn }

// SealSecret stores a one-line value sealed in the vault under name.
func SealSecret(name, value string) error {
	if strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("%s: a secret must be one line", name)
	}
	return sealer(name, value)
}

// sealDirect is the fallback when internal/secrets is not linked (providers cannot import it): no metadata prefix.
func sealDirect(name, value string) error {
	if err := vault.UpdateSealed(func(tx *vault.Tx, key *vault.Key) error {
		return tx.PutRecord(name, vault.Record{
			Sealed: key.Seal(name, value), Kid: key.Kid, Len: len(value), SetAt: time.Now(),
		})
	}); err != nil {
		return err
	}
	audit.Log(audit.SecretSet, name, "vault")
	return nil
}

// LoginU2M runs the browser login for a u2m row, seals the refresh token, mints once and returns the token's lifetime.
func LoginU2M(ctx context.Context, p *Provider, opts oauth.LoginOptions) (time.Duration, error) {
	if p.Auth.Type != AuthOAuth {
		return 0, fmt.Errorf("%s: auth.type is %q, not %s", p.Name, p.Auth.Type, AuthOAuth)
	}
	if err := checkAuthScheme(p); err != nil {
		return 0, err
	}
	host := AuthHost(p)
	if host == "" {
		return 0, fmt.Errorf("%s: no workspace host (set host or base_url)", p.Name)
	}
	tok, err := oauth.LoginU2M(ctx, host, opts)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", p.Name, err)
	}
	if tok.Refresh == "" {
		return 0, fmt.Errorf("%s: login returned no refresh token", p.Name)
	}
	err = withRefreshLock(p, func() error {
		if err := writeOAuth(p, oauthValue{Refresh: tok.Refresh, Access: tok.Access}); err != nil {
			return fmt.Errorf("%s: seal refresh token: %w", p.Name, err)
		}
		return finishCredential(p, tok.Access, tok.Expiry)
	})
	if err != nil {
		return 0, err
	}
	c, ok := cmdCache.Load(p.Name)
	if !ok {
		return 0, nil
	}
	return time.Until(c.(cached).until) + expiryMargin, nil
}
