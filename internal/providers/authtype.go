package providers

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/databricks/sdkport"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/vault"
)

// Auth types: how a bearer row mints (u2m = the SDK's databricks-cli); "" keeps env/file/command resolution.
const (
	AuthU2M      = "u2m"       // browser login once (native PKCE); sealed refresh token mints access tokens
	AuthOAuthM2M = "oauth-m2m" // client id + sealed client secret
	AuthPAT      = "pat"       // sealed personal access token
)

var secretIDRE = regexp.MustCompile(`[^A-Z0-9]+`)

// authSecretName is the vault name of p's sealed secret for its authType; "" for types with none.
func authSecretName(p *Provider) string { return SecretNameFor(p.Name, p.Auth.AuthType) }

// SecretNameFor is the vault name of the sealed secret for provider id and authType; "" for types with none.
func SecretNameFor(id, authType string) string {
	suffix := ""
	switch authType {
	case AuthU2M:
		suffix = "REFRESH_TOKEN"
	case AuthOAuthM2M:
		suffix = "CLIENT_SECRET"
	case AuthPAT:
		suffix = "PAT"
	default:
		return ""
	}
	norm := secretIDRE.ReplaceAllString(strings.ToUpper(id), "_")
	return "PROVIDER_" + strings.Trim(norm, "_") + "_" + suffix
}

// sealedAuth reports whether the authType keeps its credential in the vault.
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

// checkAuthScheme refuses a sealed authType whose host is plain http outside loopback; the error carries no credential.
func checkAuthScheme(p *Provider) error {
	u, err := url.Parse(rawAuthHost(p))
	if err != nil || u.Scheme != "http" || loopbackHost(u.Host) {
		return nil
	}
	return fmt.Errorf("%s: auth.authType %s needs an https host, got scheme %q", p.Name, p.Auth.AuthType, u.Scheme)
}

// supportedAuthTypes is the closed set validateAuthType accepts; add a value here to extend it.
var supportedAuthTypes = []string{AuthU2M, AuthOAuthM2M, AuthPAT}

var (
	accountIDRE   = regexp.MustCompile(`^[0-9a-fA-F-]{8,64}$`)
	workspaceIDRE = regexp.MustCompile(`^[0-9]+$`)
)

// validateAuthType checks auth.authType, its companion fields and the account/workspace ids for one provider.
func validateAuthType(n string, p *Provider) []string {
	var errs []string
	if id := p.Auth.AccountID; id != "" && !accountIDRE.MatchString(id) {
		errs = append(errs, fmt.Sprintf("%s: auth.accountId %q must match [0-9a-fA-F-]{8,64}", n, id))
	}
	if id := p.Auth.WorkspaceID; id != "" && !workspaceIDRE.MatchString(id) {
		errs = append(errs, fmt.Sprintf("%s: auth.workspaceId %q must be digits only", n, id))
	}
	k := p.Auth.AuthType
	if sealedAuth(k) {
		if err := checkAuthScheme(p); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if k == "" {
		if p.Auth.ClientID != "" {
			errs = append(errs, n+": auth.clientId needs auth.authType "+AuthOAuthM2M)
		}
		return errs
	}
	if !slices.Contains(supportedAuthTypes, k) {
		return append(errs, fmt.Sprintf("%s: authType %q not supported (supported: %s)", n, k, strings.Join(supportedAuthTypes, ", ")))
	}
	if p.Auth.Type != AuthBearer {
		errs = append(errs, n+": auth.authType needs auth.type bearer")
	}
	if k == AuthOAuthM2M && p.Auth.ClientID == "" {
		errs = append(errs, n+": auth.authType "+AuthOAuthM2M+" needs auth.clientId")
	}
	if k != AuthOAuthM2M && p.Auth.ClientID != "" {
		errs = append(errs, n+": auth.clientId needs auth.authType "+AuthOAuthM2M)
	}
	return errs
}

// refreshLockWait bounds how long a refresh waits for another process holding the provider's refresh lock.
const refreshLockWait = 60 * time.Second

var refreshMus sync.Map // provider name → *sync.Mutex (in-process half of the refresh lock)

var lockNameRE = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// withRefreshLock serializes a provider's refresh-token use across goroutines (mutex) and processes (file lock).
func withRefreshLock(p *Provider, fn func() error) error {
	mu, _ := refreshMus.LoadOrStore(p.Name, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()
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

// cachedValue is p's unexpired cached token, if any.
func cachedValue(p *Provider) (string, bool) {
	if c, ok := cmdCache.Load(p.Name); ok && time.Now().Before(c.(cached).until) {
		return c.(cached).value, true
	}
	return "", false
}

// authCredential mints (or serves from cache) the access token for a sealed authType; force skips the cache.
func authCredential(ctx context.Context, p *Provider, force bool) (string, error) {
	before, _ := cachedValue(p)
	if v, ok := cachedValue(p); ok && !force {
		return v, nil
	}
	if p.Auth.AuthType != AuthU2M {
		return mintAndCache(ctx, p)
	}
	var out string
	err := withRefreshLock(p, func() error {
		// Another goroutine may have minted while we waited; a forced call accepts only a newer token.
		if v, ok := cachedValue(p); ok && (!force || v != before) {
			out = v
			return nil
		}
		// mintAuth re-reads the sealed refresh token, so a rotation by another process is picked up.
		v, err := mintAndCache(ctx, p)
		out = v
		return err
	})
	return out, err
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
func mintAuth(ctx context.Context, p *Provider) (sdkport.Token, error) {
	name := authSecretName(p)
	secret, _, err := EffectiveSecret(name)
	if err != nil {
		return sdkport.Token{}, fmt.Errorf("%s: vault: %w", p.Name, err)
	}
	if secret == "" {
		return sdkport.Token{}, fmt.Errorf("%s: no credential (%s)", p.Name, authHint(p, name))
	}
	if err := checkAuthScheme(p); err != nil {
		return sdkport.Token{}, err
	}
	host := AuthHost(p)
	switch p.Auth.AuthType {
	case AuthPAT:
		return sdkport.Token{Access: secret}, nil
	case AuthOAuthM2M:
		return sdkport.MintM2M(ctx, host, p.Auth.ClientID, secret)
	}
	tok, err := sdkport.RefreshU2M(ctx, host, secret)
	if err != nil {
		return sdkport.Token{}, fmt.Errorf("%s: %w (run `claude-code databricks login %s`)", p.Name, err, p.Name)
	}
	if tok.Refresh != "" && tok.Refresh != secret {
		if err := SealSecret(name, tok.Refresh); err != nil {
			return sdkport.Token{}, fmt.Errorf("%s: re-seal rotated refresh token: %w", p.Name, err)
		}
	}
	return tok, nil
}

func authHint(p *Provider, name string) string {
	if p.Auth.AuthType == AuthU2M {
		return "run `claude-code databricks login " + p.Name + "`"
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
func LoginU2M(ctx context.Context, p *Provider, opts sdkport.LoginOptions) (time.Duration, error) {
	if p.Auth.AuthType != AuthU2M {
		return 0, fmt.Errorf("%s: auth.authType is %q, not %s", p.Name, p.Auth.AuthType, AuthU2M)
	}
	if err := checkAuthScheme(p); err != nil {
		return 0, err
	}
	host := AuthHost(p)
	if host == "" {
		return 0, fmt.Errorf("%s: no workspace host (set host or base_url)", p.Name)
	}
	tok, err := sdkport.LoginU2M(ctx, host, opts)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", p.Name, err)
	}
	if tok.Refresh == "" {
		return 0, fmt.Errorf("%s: login returned no refresh token", p.Name)
	}
	err = withRefreshLock(p, func() error {
		if err := SealSecret(authSecretName(p), tok.Refresh); err != nil {
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

// AuthSecretName exports the sealed secret name for a row's authType (display only).
func AuthSecretName(p *Provider) string { return authSecretName(p) }
