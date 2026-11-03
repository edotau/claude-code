package providers

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/databricks/oauth"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/secretstore"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/vault"
)

// ErrNeedsLogin matches (errors.Is) a u2m credential that only a fresh browser login can restore.
var ErrNeedsLogin = errors.New("login required")

// LoginRequiredError is a u2m row with no sealed refresh token or one the token endpoint rejected.
type LoginRequiredError struct {
	err error
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
	return secretstore.Store(AuthSecretName(p), string(b))
}

// sealedAuth reports whether the auth type keeps its credential in the vault.
func sealedAuth(t string) bool { return slices.Contains(supportedAuthTypes, t) }

// AuthHost is the https://host a row talks to: the preset host, else BaseURL's origin; "" when neither parses.
func AuthHost(p *Provider) string {
	h := rawAuthHost(p)
	if u, err := url.Parse(h); err == nil && u.Scheme == "http" && !textutil.IsLoopbackHost(u.Hostname()) {
		return ""
	}
	return h
}

// rawAuthHost is AuthHost before the scheme check.
func rawAuthHost(p *Provider) string {
	if h := PresetHost(p.Host); h != "" {
		return h
	}
	u, err := url.Parse(p.BaseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// checkAuthScheme refuses a sealed auth type whose host is plain http outside loopback; the error carries no credential.
func checkAuthScheme(p *Provider) error {
	u, err := url.Parse(rawAuthHost(p))
	if err != nil || u.Scheme != "http" || textutil.IsLoopbackHost(u.Hostname()) {
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
	if _, err := ResolveIdentity(p.Identity, ""); err != nil {
		errs = append(errs, n+": "+err.Error())
	}
	if strings.ContainsAny(p.Auth.SPClientID, "\r\n[=") {
		errs = append(errs, n+": auth.spClientId must not contain CR, LF, '[' or '='")
	}
	if k != AuthOAuth && p.Auth.SPClientID != "" {
		errs = append(errs, n+": auth.spClientId needs auth.type "+AuthOAuth)
	}
	return errs
}

// refreshLockWait outlasts postToken's worst case (3 × 30s client timeout + backoff), so a follower never times out first.
const refreshLockWait = 2 * time.Minute

var errRefreshLockTimeout = errors.New("timed out waiting for another refresh to finish")

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
		return fmt.Errorf("%s: %w", p.Name, errRefreshLockTimeout)
	}
	return nil
}

// authCredential mints (or serves from cache) the access token for a sealed auth type; force skips the cache.
func authCredential(ctx context.Context, p *Provider, force bool) (string, error) {
	if p.Auth.Type != AuthOAuth {
		return mintOnce(ctx, p, force, func(ctx context.Context) (string, error) { return mintAndCache(ctx, p) })
	}
	mode, err := ResolveIdentity(p.Identity, identityFrom(ctx))
	if err != nil {
		return "", fmt.Errorf("%s: %w", p.Name, err)
	}
	if mode == IdentitySP {
		return spCredential(ctx, p, force)
	}
	v, err := userCredential(ctx, p, force)
	if err != nil && mode == IdentityAuto && p.Auth.SPClientID != "" &&
		(errors.Is(err, ErrNeedsLogin) || oauth.IsRejected(err) || oauth.IsLoginRequired(err)) {
		return spCredential(ctx, p, force)
	}
	return v, err
}

// userCredential keeps user-only flights separate from automatic SP fallback.
func userCredential(ctx context.Context, p *Provider, force bool) (string, error) {
	return mintOnce(ctx, p, force, func(ctx context.Context) (string, error) {
		var out string
		// mintAuth re-reads the sealed refresh token, so a rotation by another process is picked up.
		err := withRefreshLock(p, func() error {
			if !force {
				if v, ok := sealedAccess(p, expiryMargin); ok {
					out = v
					return finishCredential(p, v, time.Time{})
				}
			}
			v, err := mintAndCache(ctx, p)
			out = v
			if err == nil {
				sealAccess(p, v)
			} else if !force && Verdict(err) == VerdictTransient {
				if old, ok := staleAccess(p, err); ok {
					out = old
					return nil
				}
			}
			return err
		})
		return out, err
	})
}

// Identity modes for a paired Databricks profile: which credential a launch uses.
const (
	IdentityAuto = "auto" // user login first, the service principal when it is rejected or needs a login
	IdentityUser = "user" // user login only
	IdentitySP   = "sp"   // service principal only, no browser
)

// ResolveIdentity picks the mode: flagAs (--as) > the profile's stored identity > auto (user when unpaired).
func ResolveIdentity(profile, flagAs string) (string, error) {
	mode := cmp.Or(flagAs, profile, IdentityAuto)
	switch mode {
	case IdentityAuto, IdentityUser, IdentitySP:
		return mode, nil
	}
	return "", fmt.Errorf("identity %q must be auto, user or sp", mode)
}

type identityKey struct{}

// WithIdentity carries a launch's --as value on the credential request's context.
func WithIdentity(ctx context.Context, as string) context.Context {
	return context.WithValue(ctx, identityKey{}, as)
}

func identityFrom(ctx context.Context) string {
	as, _ := ctx.Value(identityKey{}).(string)
	return as
}

// Seams for the token endpoint calls and the fallback note.
var (
	refreshU2M           = oauth.RefreshU2M
	mintM2M              = oauth.MintM2M
	spNoteOut  io.Writer = os.Stderr
	spCache    sync.Map
	spFlightMu sync.Mutex
	spFlights  = map[string]*flight{}
)

// spCredential isolates SP tokens, expiry records and flights from the user's profile key.
func spCredential(ctx context.Context, p *Provider, force bool) (string, error) {
	for {
		spFlightMu.Lock()
		if !force {
			if c, ok := spCache.Load(p.Name); ok && time.Now().Before(c.(cached).until) {
				spFlightMu.Unlock()
				return c.(cached).value, nil
			}
		}
		f, inflight := spFlights[p.Name]
		if !inflight {
			f = &flight{done: make(chan struct{}), force: force}
			spFlights[p.Name] = f
			spFlightMu.Unlock()
			defer func() {
				spFlightMu.Lock()
				delete(spFlights, p.Name)
				close(f.done)
				spFlightMu.Unlock()
			}()
			f.val, f.err = mintSP(context.WithoutCancel(ctx), p)
			return f.val, f.err
		}
		spFlightMu.Unlock()
		select {
		case <-f.done:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		if f.force || !force {
			return f.val, f.err
		}
	}
}

// mintSP mints the paired service principal's token (same workspace host); it never touches the user login.
func mintSP(ctx context.Context, p *Provider) (string, error) {
	if p.Auth.SPClientID == "" {
		return "", fmt.Errorf("%s: identity sp needs a paired service principal (auth.spClientId)", p.Name)
	}
	name := SecretName(p.provider(), p.Name, AuthM2M)
	secret, _, err := secretstore.EffectiveSecret(name)
	if err != nil {
		return "", fmt.Errorf("%s: vault: %w", p.Name, err)
	}
	if secret == "" {
		return "", fmt.Errorf("%s: no service-principal credential (set %s with `claude-code vault set`)", p.Name, name)
	}
	tok, err := mintM2M(ctx, AuthHost(p), p.Auth.SPClientID, secret)
	if err != nil {
		return "", fmt.Errorf("%s: service principal: %w", p.Name, err)
	}
	if err := finishSPCredential(p, tok.Access, tok.Expiry); err != nil {
		return "", err
	}
	fmt.Fprintf(spNoteOut, "%s: using the service principal %s\n", p.Name, p.Auth.SPClientID)
	return tok.Access, nil
}

// finishSPCredential caches the SP token apart from the user profile's cache.
func finishSPCredential(p *Provider, v string, exp time.Time) error {
	if err := checkIssuer(p, v); err != nil {
		return err
	}
	ttl := time.Duration(p.Auth.TTL) * time.Second
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	until := time.Now().Add(ttl)
	if e, ok := jwtExp(v); ok && exp.IsZero() {
		exp = time.Unix(e, 0)
	}
	if !exp.IsZero() && exp.Add(-expiryMargin).Before(until) {
		until = exp.Add(-expiryMargin)
	}
	spCache.Store(p.Name, cached{value: v, until: until})
	return nil
}

// staleAccess rides out a transient refresh failure on the sealed access token for one-minute cache windows.
func staleAccess(p *Provider, err error) (string, bool) {
	old, ok := sealedAccess(p, time.Minute)
	if !ok {
		return "", false
	}
	fmt.Fprintf(os.Stderr, "%s: refresh failed (%s); using the cached token until it expires\n", p.Name, textutil.FirstLine(err.Error()))
	cmdCache.Store(p.Name, cached{value: old, until: time.Now().Add(time.Minute)})
	return old, true
}

// sealedAccess returns the sealed access token when it stays valid past margin.
func sealedAccess(p *Provider, margin time.Duration) (string, bool) {
	ov, err := readOAuth(p)
	v := strings.TrimSpace(ov.Access)
	if err != nil || v == "" {
		return "", false
	}
	exp, ok := jwtExp(v)
	if !ok || !time.Unix(exp, 0).Add(-margin).After(time.Now()) {
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
			err = &LoginRequiredError{err: err}
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
		return mintM2M(ctx, host, p.Auth.ClientID, secret)
	}
	tok, err := refreshU2M(ctx, host, secret)
	if err != nil {
		wrapped := fmt.Errorf("%s: %w (run `claude-code databricks login --profile %s`)", p.Name, err, p.Name)
		if oauth.IsLoginRequired(err) {
			wrapped = &LoginRequiredError{err: wrapped}
		}
		return oauth.Token{}, wrapped
	}
	// A carried in-memory token is re-sealed on every mint until the vault takes it.
	if _, carried := unsealedRefresh.Load(p.Name); carried || (tok.Refresh != "" && tok.Refresh != secret) {
		if err := sealRotated(p, oauth.Token{Refresh: cmp.Or(tok.Refresh, secret), Access: tok.Access}); err != nil {
			fmt.Fprintf(os.Stderr, "%s: re-seal rotated refresh token (%s) failed; kept in memory: %v\n", p.Name, name, err)
		}
	}
	return tok, nil
}

// unsealedRefresh maps a provider name to a rotated refresh token whose re-seal failed; mintSecret prefers it.
var unsealedRefresh sync.Map

// sealRetryDelay is the backoff unit between re-seal attempts (1x, then 2x).
var sealRetryDelay = time.Second

// sealRotated retries the re-seal: a lost rotated refresh token forces a browser login once the old one is spent.
func sealRotated(p *Provider, tok oauth.Token) error {
	var err error
	for i := range 3 {
		if err = writeOAuth(p, oauthValue{Refresh: tok.Refresh, Access: tok.Access}); err == nil {
			unsealedRefresh.Delete(p.Name)
			return nil
		}
		if i < 2 {
			time.Sleep(time.Duration(i+1) * sealRetryDelay)
		}
	}
	unsealedRefresh.Store(p.Name, tok.Refresh)
	return err
}

// mintSecret reads the sealed secret; u2m reads the vault only so a stale env value never replays a rotated token.
func mintSecret(p *Provider) (string, error) {
	if p.Auth.Type == AuthOAuth {
		if v, ok := unsealedRefresh.Load(p.Name); ok {
			return v.(string), nil
		}
		ov, err := readOAuth(p)
		return strings.TrimSpace(ov.Refresh), err
	}
	v, _, err := secretstore.EffectiveSecret(AuthSecretName(p))
	return v, err
}

func authHint(p *Provider, name string) string {
	if p.Auth.Type == AuthOAuth {
		return "run `claude-code databricks login --profile " + p.Name + "`, or `claude-code configure` and pick Set up"
	}
	return "set " + name + " with `claude-code vault set`"
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
	// A fresh login supersedes the lock holder's refresh, so a lock timeout still seals it.
	seal := func() error {
		if err := writeOAuth(p, oauthValue{Refresh: tok.Refresh, Access: tok.Access}); err != nil {
			return fmt.Errorf("%s: seal refresh token: %w", p.Name, err)
		}
		unsealedRefresh.Delete(p.Name)
		return finishCredential(p, tok.Access, tok.Expiry)
	}
	if err = withRefreshLock(p, seal); errors.Is(err, errRefreshLockTimeout) {
		err = seal()
	}
	if err != nil {
		return 0, err
	}
	c, ok := cmdCache.Load(p.Name)
	if !ok {
		return 0, nil
	}
	return time.Until(c.(cached).until) + expiryMargin, nil
}

// Auth types, the one auth.type value on disk and in memory.
const (
	AuthOAuth = "oauth" // browser login once; sealed {refresh, access}
	AuthM2M   = "m2m"   // client id + sealed client secret
	AuthPAT   = "pat"   // sealed personal access token
	AuthKey   = "key"   // a static key sent in a header
	AuthCLI   = "cli"   // the agent CLI's own sign-in
	AuthNone  = "none"  // no credential
)

// Header styles for auth.header.
const (
	HeaderXAPIKey = "x-api-key"
	HeaderBearer  = "bearer"
)

// resolveKeyHeader fills a key row's header from the dialect: anthropic sends x-api-key, the rest bearer.
func (a *Auth) resolveKeyHeader(kind string) {
	if a.Type != AuthKey || a.Header != "" {
		return
	}
	a.Header = HeaderBearer
	if kind == ProtocolAnthropic {
		a.Header = HeaderXAPIKey
	}
}

// Method is the row's auth.type: oauth m2m pat key cli none; "" when unset.
func (a Auth) Method() string { return a.Type }

// HeaderStyle is the header a row sends its token in (x-api-key or bearer); "" for cli and none.
func (a Auth) HeaderStyle() string {
	switch a.Type {
	case AuthOAuth, AuthM2M, AuthPAT, AuthKey:
		if a.Header != "" {
			return a.Header
		}
		return HeaderBearer
	}
	return ""
}

// Sealed reports whether the credential lives in the vault (oauth, m2m or pat).
func (a Auth) Sealed() bool { return sealedAuth(a.Type) }

// AuthVerdict is what an auth failure means for routing: only needs_login and rejected condemn the account.
type AuthVerdict string

const (
	VerdictOK         AuthVerdict = "ok"
	VerdictNeedsLogin AuthVerdict = "needs_login"
	VerdictRejected   AuthVerdict = "rejected"
	VerdictTransient  AuthVerdict = "transient"
)

// Verdict classifies a mint/refresh error; transport, timeout, 429 and 5xx are transient and never condemn.
func Verdict(err error) AuthVerdict {
	switch {
	case err == nil:
		return VerdictOK
	case errors.Is(err, ErrNeedsLogin) || oauth.IsLoginRequired(err):
		return VerdictNeedsLogin
	case oauth.IsRejected(err):
		return VerdictRejected
	}
	return VerdictTransient
}

// Condemns reports whether the verdict should mark the account unusable until a login or credential change.
func (v AuthVerdict) Condemns() bool { return v == VerdictNeedsLogin || v == VerdictRejected }

// IsInvalidTokenResponse reports the AI Gateway's 400 "Invalid Token" on a Databricks row (a ≤256-byte body peek).
func IsInvalidTokenResponse(p *Provider, status int, body []byte) bool {
	if p == nil || (p.Preset != PresetDatabricks && !sealedAuth(p.Auth.Type)) {
		return false
	}
	return status == http.StatusBadRequest && bytes.Contains(body, []byte(`"Invalid Token"`))
}
