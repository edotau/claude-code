package providers

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

	bolterrors "go.etcd.io/bbolt/errors"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/databricks/oauth"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/vault"
)

// ErrNeedsLogin matches (errors.Is) a u2m credential that only a fresh browser login can restore.
var ErrNeedsLogin = errors.New("login required")

// loginRequiredError is a u2m row with no sealed refresh token or one the token endpoint rejected.
type loginRequiredError struct {
	err error
}

func (e *loginRequiredError) Error() string { return e.err.Error() }
func (e *loginRequiredError) Unwrap() error { return e.err }

// Is makes errors.Is(err, ErrNeedsLogin) true.
func (e *loginRequiredError) Is(target error) bool { return target == ErrNeedsLogin }

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
	return SecretName(p.Provider, p.Name, p.Auth.Method())
}

// keySecretName is the vault name of a key row's static key; "" for any other row.
func keySecretName(p *Provider) string {
	if p.Auth.Method() != AuthKey {
		return ""
	}
	return SecretName(p.Provider, p.Name, AuthKey)
}

// oauthValue is an oauth profile's one sealed value: the refresh and the cached access token, and when it was sealed.
type oauthValue struct {
	Refresh string `json:"refresh,omitempty"`
	Access  string `json:"access,omitempty"`
	Sealed  int64  `json:"sealed,omitempty"` // unix nanos of the write; 0 on a record sealed before the stamp existed
}

// readOAuth reads p's sealed {refresh, access}; a vault lock timeout is retried once.
func readOAuth(p *Provider) (oauthValue, error) {
	var ov oauthValue
	raw, found, err := vault.ReadSecret(AuthSecretName(p))
	if errors.Is(err, bolterrors.ErrTimeout) {
		raw, found, err = vault.ReadSecret(AuthSecretName(p))
	}
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

// writeOAuth seals ov, stamped now, as the one JSON value under p's oauth secret name; a sealed record ends a dead mark.
func writeOAuth(p *Provider, ov oauthValue) error {
	ov.Sealed = time.Now().UnixNano()
	b, err := json.Marshal(ov)
	if err != nil {
		return err
	}
	if err := vault.Store(AuthSecretName(p), string(b)); err != nil {
		return err
	}
	clearLoginMark(p.Name)
	return nil
}

// sealedAuth reports whether the auth type keeps its credential in the vault.
func sealedAuth(t string) bool { return slices.Contains(supportedAuthTypes, t) }

// AuthHost is the https://host a row talks to: the preset host, else BaseURL's origin; "" when neither parses.
func AuthHost(p *Provider) string {
	h := rawAuthHost(p)
	if u, err := url.Parse(h); err == nil && textutil.IsPlainHTTPOffLoopback(u) {
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
	if err != nil || !textutil.IsPlainHTTPOffLoopback(u) {
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
	errs := validateAuthIDs(n, p)
	k := p.Auth.Type
	if sealedAuth(k) {
		if err := checkAuthScheme(p); err != nil {
			errs = append(errs, err.Error())
		}
	}
	errs = append(errs, validateClientID(n, p)...)
	if _, err := ResolveIdentity(p.Identity, ""); err != nil {
		errs = append(errs, n+": "+err.Error())
	}
	return append(errs, validateSPClientID(n, p)...)
}

// validateAuthIDs checks the account and workspace id shapes.
func validateAuthIDs(n string, p *Provider) []string {
	var errs []string
	if id := p.Auth.AccountID; id != "" && !accountIDRE.MatchString(id) {
		errs = append(errs, fmt.Sprintf("%s: auth.accountId %q must match [0-9a-fA-F-]{8,64}", n, id))
	}
	if id := p.Auth.WorkspaceID; id != "" && !workspaceIDRE.MatchString(id) {
		errs = append(errs, fmt.Sprintf("%s: auth.workspaceId %q must be digits only", n, id))
	}
	return errs
}

// validateClientID checks auth.clientId against the m2m type and its allowed characters.
func validateClientID(n string, p *Provider) []string {
	var errs []string
	k := p.Auth.Type
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

// validateSPClientID checks auth.spClientId against the oauth type and its allowed characters.
func validateSPClientID(n string, p *Provider) []string {
	var errs []string
	if strings.ContainsAny(p.Auth.SPClientID, "\r\n[=") {
		errs = append(errs, n+": auth.spClientId must not contain CR, LF, '[' or '='")
	}
	if p.Auth.Type != AuthOAuth && p.Auth.SPClientID != "" {
		errs = append(errs, n+": auth.spClientId needs auth.type "+AuthOAuth)
	}
	return errs
}

// refreshLockWait outlasts postToken's worst case (3 × 30s client timeout + backoff), so a follower never times out first.
var refreshLockWait = 2 * time.Minute // a var so tests can shorten it

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
		return mintOnce(ctx, p, force, func(ctx context.Context) (string, error) {
			v, _, err := mintAndCache(ctx, p, force)
			return v, err
		})
	}
	as := IdentityFrom(ctx)
	mode, err := ResolveIdentity(p.Identity, headlessAs(as))
	if err != nil {
		return "", fmt.Errorf("%s: %w", p.Name, err)
	}
	switch {
	case mode == IdentityAuto && as == IdentityHeadless:
		return dispatchCredential(ctx, p, force)
	case mode == IdentityAuto:
		return autoCredential(ctx, p, force)
	case mode == IdentitySP:
		return spCredential(ctx, p, force)
	}
	return userCredential(ctx, p, force)
}

// autoCredential is auto outside a dispatch: the user login, the paired SP while none is stored or when it is refused
// (the fallback is reported once per dead-login window).
func autoCredential(ctx context.Context, p *Provider, force bool) (string, error) {
	if p.Auth.SPClientID != "" {
		if v, handled, err := autoSPFirst(ctx, p, force); handled {
			return v, err
		}
	}
	v, err := userCredential(ctx, p, force)
	if err != nil && p.Auth.SPClientID != "" && isLoginFailure(err) {
		if sp, spErr := fallbackToSP(ctx, p, force); spErr == nil {
			return sp, nil
		}
		return "", err // the login error names the command that fixes it
	}
	return spOnVaultError(ctx, p, force, v, err)
}

// autoSPFirst serves the paired SP before any user attempt when no login is stored or a dead login's window is open.
func autoSPFirst(ctx context.Context, p *Provider, force bool) (string, bool, error) {
	if !hasUserLogin(p) {
		v, err := spCredential(ctx, p, force)
		return v, true, err
	}
	if _, dead := openLoginMark(p); dead && SPPresent(p) { // no user attempt while the dead login's window is open
		v, err := fallbackToSP(ctx, p, force)
		return v, true, err
	}
	return "", false, nil
}

// isLoginFailure reports a user-login failure the paired SP may cover: no login, or one the IdP refused.
func isLoginFailure(err error) bool {
	return errors.Is(err, ErrNeedsLogin) || oauth.IsRejected(err) || oauth.IsLoginRequired(err)
}

// isVaultError reports a failure reading the vault (a lock timeout, no key), as opposed to a login the IdP refused.
func isVaultError(p *Provider, err error) bool {
	return errors.Is(err, bolterrors.ErrTimeout) || strings.HasPrefix(err.Error(), p.Name+": vault:")
}

// spOnVaultError serves the paired SP when the user path failed on the vault; the SP may be readable or env-held.
func spOnVaultError(ctx context.Context, p *Provider, force bool, v string, err error) (string, error) {
	if err == nil || p.Auth.SPClientID == "" || !isVaultError(p, err) {
		return v, err
	}
	sp, spErr := spCredential(ctx, p, force)
	if spErr != nil {
		return v, err
	}
	note(ctx, p.Name+": your login could not be read from the vault; using the service principal")
	return sp, nil
}

// hasUserLogin reports a sealed (or carried) user login; a vault read error counts as one so the user path reports it.
func hasUserLogin(p *Provider) bool {
	if _, carried := unsealedRefresh.Load(p.Name); carried {
		return true
	}
	ov, err := readOAuth(p)
	return err != nil || ov != oauthValue{}
}

// spSkipWait is how long a dispatch goes straight to the user login after its service principal was unavailable.
const spSkipWait = 10 * time.Minute

var spSkip sync.Map // profile name -> time.Time until which dispatches skip its unavailable service principal

// dispatchCredential is auto for a headless dispatch: the paired SP first; a missing secret or a rejected mint falls
// back to the user login with one note, and later dispatches skip that SP for spSkipWait unless forced.
func dispatchCredential(ctx context.Context, p *Provider, force bool) (string, error) {
	if until, ok := spSkip.Load(p.Name); p.Auth.SPClientID == "" || (ok && !force && time.Now().Before(until.(time.Time))) {
		v, err := userCredential(ctx, p, force)
		return spOnVaultError(ctx, p, force, v, err)
	}
	reason := "no service-principal secret stored"
	if present, readErr := spPresence(p); present || readErr != nil { // a vault read error is unknown, not absent
		v, err := spCredential(ctx, p, force)
		if err == nil || !oauth.IsRejected(err) {
			return v, err
		}
		reason = redactDetail(err.Error())
	}
	spSkip.Store(p.Name, time.Now().Add(spSkipWait))
	note(ctx, fmt.Sprintf("%s: service principal unavailable (%s); falling back to your own login", p.Name, reason))
	return userCredential(ctx, p, force)
}

// userCredential keeps user-only flights separate from automatic SP fallback.
// While a dead login's retry window is open an unforced call fails at once, with no lock and no network.
func userCredential(ctx context.Context, p *Provider, force bool) (string, error) {
	return mintOnce(ctx, p, force, func(ctx context.Context) (string, error) {
		if rec, dead := openLoginMark(p); dead && !force {
			return "", loginRenewalError(p, rec)
		}
		out, _, err := lockedUserMint(ctx, p, force, expiryMargin)
		return out, err
	})
}

// lockedUserMint is userMint under p's refresh lock; minted reports a token endpoint exchange.
func lockedUserMint(ctx context.Context, p *Provider, force bool, margin time.Duration) (out string, minted bool, err error) {
	err = withRefreshLock(p, func() error {
		var e error
		out, minted, e = userMint(ctx, p, force, margin)
		return e
	})
	return out, minted, err
}

// userMint serves a peer's sealed token valid past margin, else exchanges the sealed refresh token (re-read under the
// lock, so a rotation by another process is picked up) and marks or clears a dead login.
func userMint(ctx context.Context, p *Provider, force bool, margin time.Duration) (string, bool, error) {
	if v, ok := peerToken(func(m time.Duration) (string, bool) { return sealedAccess(p, m) }, &cmdCache, p.Name, force, margin); ok {
		return v, false, finishCredential(p, v, time.Time{})
	}
	v, base, err := mintAndCache(ctx, p, force)
	if err != nil && Verdict(err).Condemns() && sealedSince(p, base) { // a login sealed while the IdP judged the older token
		v, base, err = mintAndCache(ctx, p, force)
	}
	if err == nil {
		clearLoginMark(p.Name)
		sealAccess(p, v, base)
		return v, true, nil
	}
	markLoginDead(p, err)
	if !force && Verdict(err) == VerdictTransient {
		if old, ok := staleAccess(p, err); ok {
			return old, false, nil
		}
	}
	return "", false, err
}

// Seams for the token endpoint calls and the fallback note.
var (
	refreshU2M           = oauth.RefreshU2M
	mintM2M              = oauth.MintM2M
	spNoteOut  io.Writer = os.Stderr
	spCache    sync.Map
	spFlights  = &flightGroup{flights: map[string]*flight{}, cached: func(name string) (string, bool) { return cacheGet(&spCache, name) }}
)

// spCredential isolates SP tokens, expiry records and flights from the user's profile key.
func spCredential(ctx context.Context, p *Provider, force bool) (string, error) {
	return spFlights.do(ctx, p.Name, force, func(ctx context.Context) (string, error) {
		v, _, err := mintSP(ctx, p, force, expiryMargin)
		return v, err
	})
}

// mintSP mints the paired service principal's token (same workspace host); it never touches the user login.
// The mint holds the profile's refresh lock and re-checks the sealed slot, so a follower serves the winner's token;
// a sealed token valid past margin is served, and minted reports a token endpoint exchange.
func mintSP(ctx context.Context, p *Provider, force bool, margin time.Duration) (out string, minted bool, err error) {
	name, secret, access, err := spSecrets(p)
	if err != nil {
		return "", false, err
	}
	host := AuthHost(p)
	bind := clientBind(host, p.Auth.SPClientID, secret)
	if v, ok := clientAccessHit(access, bind, margin); ok && !force {
		return v, false, finishSPCredential(p, v, time.Time{}) // a served token is no news: the note marks a mint only
	}
	mint := func() error {
		var e error
		out, minted, e = spMintUnderLock(ctx, p, force, margin, name, host, secret, access, bind)
		return e
	}
	ran := false
	err = withRefreshLock(p, func() error { ran = true; return mint() })
	if !ran { // an SP mint is idempotent: a lock timeout or lock error mints unlocked instead of failing
		err = mint()
	}
	if err != nil {
		return "", false, err
	}
	return out, minted, nil
}

// spMintUnderLock serves a peer's sealed SP token, else mints one (riding a transient failure on the stale token).
func spMintUnderLock(ctx context.Context, p *Provider, force bool, margin time.Duration, name, host, secret, access, bind string) (out string, minted bool, err error) {
	read := func(m time.Duration) (string, bool) { return sealedClientAccess(name+"_ACCESS", bind, m) }
	if v, ok := peerToken(read, &spCache, p.Name, force, margin); ok {
		return v, false, finishSPCredential(p, v, time.Time{})
	}
	tok, err := m2mToken(ctx, p, name, host, p.Auth.SPClientID, secret, true) // the sealed slot was checked above
	if err != nil {
		if !force && Verdict(err) == VerdictTransient {
			if old, ok := staleSPAccess(p, access, bind, err); ok {
				return old, false, nil
			}
		}
		return "", false, fmt.Errorf("%s: service principal: %w", p.Name, err)
	}
	if err := finishSPCredential(p, tok.Access, tok.Expiry); err != nil {
		return "", false, err
	}
	note(ctx, fmt.Sprintf("%s: using the service principal %s", p.Name, p.Auth.SPClientID))
	return tok.Access, true, nil
}

// peerToken is the sealed token another process stored that this call may serve instead of minting. Unforced: any one
// valid past margin. Forced: one valid for a minute that differs from the token this process last served
// (the one the caller just saw rejected); a process that served none mints.
func peerToken(read func(margin time.Duration) (string, bool), cache *sync.Map, name string, force bool, margin time.Duration) (string, bool) {
	if !force {
		return read(margin)
	}
	c, ok := cache.Load(name)
	if !ok {
		return "", false
	}
	if v, ok := read(time.Minute); ok && v != c.(cached).value {
		return v, true
	}
	return "", false
}

// spSecrets reads the paired service principal's vault name, client secret and sealed access slot.
func spSecrets(p *Provider) (name, secret, access string, err error) {
	if p.Auth.SPClientID == "" {
		return "", "", "", fmt.Errorf("%s: identity sp needs a paired service principal (auth.spClientId)", p.Name)
	}
	name = SecretName(p.Provider, p.Name, AuthM2M)
	secret, access, err = clientSecrets(name)
	if err != nil {
		return "", "", "", fmt.Errorf("%s: vault: %w", p.Name, err)
	}
	if secret == "" {
		return "", "", "", fmt.Errorf("%s: no service-principal credential (set %s with `claude-code vault set`)", p.Name, name)
	}
	return name, secret, access, nil
}

// clientSecrets reads a client secret (env first, like vault.EffectiveSecret) and its sealed _ACCESS slot in one read.
func clientSecrets(name string) (secret, access string, err error) {
	vals, err := vault.ReadSecrets([]string{name, name + "_ACCESS"})
	if err != nil { // an unreadable cache slot must not block the mint: read the secret alone
		secret, _, err = vault.EffectiveSecret(name)
		return secret, "", err
	}
	return cmp.Or(strings.TrimSpace(os.Getenv(name)), vals[name]), vals[name+"_ACCESS"], nil
}

// finishSPCredential caches the SP token apart from the user profile's cache.
func finishSPCredential(p *Provider, v string, exp time.Time) error {
	return cacheCredential(p, &spCache, p.Name+spExpirySuffix, AuthM2M, v, exp)
}

// staleAccess rides out a transient refresh failure on the sealed access token for one-minute cache windows.
func staleAccess(p *Provider, err error) (string, bool) {
	old, _ := sealedAccess(p, time.Minute)
	return staleServe(p, &cmdCache, "refresh", old, err)
}

// staleSPAccess is staleAccess for the SP: a transient mint failure rides on the sealed _ACCESS token.
func staleSPAccess(p *Provider, access, bind string, err error) (string, bool) {
	old, _ := clientAccessHit(access, bind, time.Minute)
	return staleServe(p, &spCache, "service principal mint", old, err)
}

// sealedAccess returns the sealed access token when it stays valid past margin.
func sealedAccess(p *Provider, margin time.Duration) (string, bool) {
	ov, err := readOAuth(p)
	v := strings.TrimSpace(ov.Access)
	if err != nil || v == "" {
		return "", false
	}
	return jwtValidPast(v, margin)
}

// sealAccess caches a u2m access token beside the refresh token unless a record newer than base is sealed; a seal error
// is logged by name only.
func sealAccess(p *Provider, tok string, base int64) {
	if _, carried := unsealedRefresh.Load(p.Name); carried { // a rewrite would stamp over the carry
		return
	}
	ov, err := readOAuth(p)
	if err != nil { // a failed read must not re-seal {access} alone and drop the refresh token
		fmt.Fprintf(os.Stderr, "%s: cache access token (%s) skipped: vault read failed\n", p.Name, AuthSecretName(p))
		audit.Log(audit.SealFailed, AuthSecretName(p), "access")
		return
	}
	if ov.Access == tok || ov.Sealed > base { // the rotation path sealed this token, or another login won meanwhile
		return
	}
	ov.Access = tok
	if err := writeOAuth(p, ov); err != nil {
		fmt.Fprintf(os.Stderr, "%s: cache access token (%s) failed\n", p.Name, AuthSecretName(p))
		audit.Log(audit.SealFailed, AuthSecretName(p), "access")
	}
}

// sealedSince reports whether the vault holds a record sealed after stamp; a missing stamp is older than any.
func sealedSince(p *Provider, stamp int64) bool {
	ov, err := readOAuth(p)
	return err == nil && ov.Sealed > stamp
}

// mintAndCache mints, caches and returns the access token with the vault stamp the mint started from, on failure too.
func mintAndCache(ctx context.Context, p *Provider, force bool) (string, int64, error) {
	tok, base, err := mintAuth(ctx, p, force)
	if err != nil {
		return "", base, err
	}
	if err := finishCredential(p, tok.Access, tok.Expiry); err != nil {
		return "", 0, err
	}
	return tok.Access, base, nil
}

// mintAuth is the one path that turns a sealed credential into an access token; base is the vault stamp it read.
func mintAuth(ctx context.Context, p *Provider, force bool) (oauth.Token, int64, error) {
	name := AuthSecretName(p)
	secret, base, err := mintSource(p)
	if err != nil {
		return oauth.Token{}, 0, fmt.Errorf("%s: vault: %w", p.Name, err)
	}
	if secret == "" {
		return oauth.Token{}, 0, noSecretError(p, name)
	}
	if err := checkAuthScheme(p); err != nil {
		return oauth.Token{}, 0, err
	}
	host := AuthHost(p)
	switch p.Auth.Type {
	case AuthPAT:
		return oauth.Token{Access: secret}, base, nil
	case AuthM2M:
		tok, err := m2mToken(ctx, p, name, host, p.Auth.ClientID, secret, force)
		return tok, base, err
	}
	tok, err := mintU2M(ctx, p, host, name, secret, base)
	return tok, base, err
}

// clientAccess is a client-credentials token sealed for other processes; Bind ties it to the host, client and secret.
type clientAccess struct {
	Access string `json:"access"`
	Bind   string `json:"bind"`
}

// m2mToken serves an m2m/sp token sealed by any process while it is valid past expiryMargin; force mints anew.
func m2mToken(ctx context.Context, p *Provider, secretName, host, clientID, secret string, force bool) (oauth.Token, error) {
	bind := clientBind(host, clientID, secret)
	name := secretName + "_ACCESS" // per row on purpose: a forced mint rotates only that row's slot
	if !force {
		if v, ok := sealedClientAccess(name, bind, expiryMargin); ok {
			return oauth.Token{Access: v}, nil
		}
	}
	tok, err := mintM2M(ctx, host, clientID, secret)
	if err != nil {
		return tok, err
	}
	if err := checkIssuer(p, tok.Access); err != nil { // a wrong-issuer token must not reach the shared vault
		return tok, err
	}
	if b, err := json.Marshal(clientAccess{Access: tok.Access, Bind: bind}); err == nil {
		if err := vault.Store(name, string(b)); err != nil {
			fmt.Fprintf(os.Stderr, "cache access token (%s) failed\n", name)
			kind := "access"
			if p.Auth.Type == AuthOAuth {
				kind = "sp-access"
			}
			audit.Log(audit.SealFailed, name, kind)
		}
	}
	return tok, nil
}

// clientBind ties a sealed client token to its host, client id and secret without storing the secret.
func clientBind(host, clientID, secret string) string {
	sum := sha256.Sum256([]byte(host + "\x00" + clientID + "\x00" + secret))
	return hex.EncodeToString(sum[:8])
}

// sealedClientAccess returns the sealed token under name when bind matches and it stays valid past margin.
func sealedClientAccess(name, bind string, margin time.Duration) (string, bool) {
	raw, _, err := vault.ReadSecret(name)
	if err != nil {
		return "", false
	}
	return clientAccessHit(raw, bind, margin)
}

// clientAccessToken is the token in a sealed clientAccess when bind matches, whatever its age; "" otherwise.
func clientAccessToken(raw, bind string) string {
	var ca clientAccess
	if raw == "" || json.Unmarshal([]byte(raw), &ca) != nil || ca.Bind != bind {
		return ""
	}
	return ca.Access
}

// clientAccessHit returns the token in a sealed clientAccess when bind matches and it stays valid past margin.
func clientAccessHit(raw, bind string, margin time.Duration) (string, bool) {
	return jwtValidPast(clientAccessToken(raw, bind), margin)
}

// noSecretError is the missing-credential error; an oauth row's is a loginRequiredError.
func noSecretError(p *Provider, name string) error {
	err := fmt.Errorf("%s: no credential (%s)", p.Name, authHint(p, name))
	if p.Auth.Type == AuthOAuth {
		err = &loginRequiredError{err: err}
	}
	return err
}

// mintU2M exchanges the sealed refresh token and re-seals a rotated one; base is the vault stamp secret came from.
func mintU2M(ctx context.Context, p *Provider, host, name, secret string, base int64) (oauth.Token, error) {
	tok, err := refreshU2M(ctx, host, secret)
	if err != nil {
		wrapped := fmt.Errorf("%s: %w (%s)", p.Name, err, authHint(p, name))
		if oauth.IsLoginRequired(err) {
			wrapped = &loginRequiredError{err: wrapped}
		}
		return oauth.Token{}, wrapped
	}
	// A carried in-memory token is re-sealed on every mint until the vault takes it.
	if _, carried := unsealedRefresh.Load(p.Name); carried || (tok.Refresh != "" && tok.Refresh != secret) {
		if err := sealRotated(ctx, p, oauth.Token{Refresh: cmp.Or(tok.Refresh, secret), Access: tok.Access}, base); err != nil {
			fmt.Fprintf(os.Stderr, "%s: re-seal rotated refresh token (%s) failed; kept in memory: %v\n", p.Name, name, err)
		}
	}
	return tok, nil
}

// carriedRefresh is a rotated refresh token whose re-seal failed, and the vault stamp of the record it came from.
type carriedRefresh struct {
	token string
	base  int64
}

// unsealedRefresh maps a provider name to its carriedRefresh; mintSource prefers it until the vault holds a newer record.
var unsealedRefresh sync.Map

// sealRetryDelay is the backoff unit between re-seal attempts (1x, then 2x).
var sealRetryDelay = time.Second

// sealRotated retries the re-seal: a lost rotated refresh token forces a browser login once the old one is spent.
func sealRotated(ctx context.Context, p *Provider, tok oauth.Token, base int64) error {
	var err error
	for i := range 3 {
		if err = sealUnlessNewer(p, tok, base); err == nil {
			return nil
		}
		if i < 2 {
			select {
			case <-ctx.Done():
				return carryRotated(p, tok.Refresh, base, errors.Join(err, ctx.Err()))
			case <-time.After(time.Duration(i+1) * sealRetryDelay):
			}
		}
	}
	return carryRotated(p, tok.Refresh, base, err)
}

// carryRotated keeps a refresh token the vault would not take for the next mint and leaves an audit trace.
func carryRotated(p *Provider, refresh string, base int64, err error) error {
	unsealedRefresh.Store(p.Name, carriedRefresh{token: refresh, base: base})
	audit.Log(audit.SealFailed, AuthSecretName(p), "refresh")
	return err
}

// sealUnlessNewer seals tok unless the vault record is newer than base (another login or rotation won): then it keeps
// that record and drops any carry. Callers hold the profile's refresh lock, which makes read-then-write safe.
func sealUnlessNewer(p *Provider, tok oauth.Token, base int64) error {
	ov, err := readOAuth(p)
	if err != nil {
		return err
	}
	if ov.Sealed > base {
		unsealedRefresh.Delete(p.Name)
		return nil
	}
	if err := writeOAuth(p, oauthValue{Refresh: tok.Refresh, Access: tok.Access}); err != nil {
		return err
	}
	unsealedRefresh.Delete(p.Name)
	return nil
}

// mintSecret is mintSource without the stamp.
func mintSecret(p *Provider) (string, error) {
	secret, _, err := mintSource(p)
	return secret, err
}

// mintSource reads the sealed secret and, for u2m, the stamp of the record it came from. u2m reads the vault only so a
// stale env value never replays a rotated token; a carried refresh wins only while the vault holds nothing newer.
func mintSource(p *Provider) (secret string, base int64, err error) {
	if p.Auth.Type != AuthOAuth {
		secret, _, err = vault.EffectiveSecret(AuthSecretName(p))
		return secret, 0, err
	}
	ov, err := readOAuth(p)
	if v, ok := unsealedRefresh.Load(p.Name); ok {
		c := v.(carriedRefresh)
		if err != nil || ov.Sealed <= c.base { // an unreadable vault cannot show a newer record; sealUnlessNewer rechecks
			return c.token, c.base, nil
		}
		unsealedRefresh.Delete(p.Name)
	}
	return strings.TrimSpace(ov.Refresh), ov.Sealed, err
}

// authHint is the one fix-it phrase for a missing or rejected credential: a login for oauth, else the vault secret.
func authHint(p *Provider, name string) string {
	if p.Auth.Type == AuthOAuth {
		return "run `claude-code databricks login --profile " + p.Name + "`, or `claude-code configure` and pick Set up"
	}
	return "set " + name + " with `claude-code vault set`"
}

// loginHost checks that p is a u2m row with an https (or loopback) workspace host and returns that host.
func loginHost(p *Provider) (string, error) {
	if p.Auth.Type != AuthOAuth {
		return "", fmt.Errorf("%s: auth.type is %q, not %s", p.Name, p.Auth.Type, AuthOAuth)
	}
	if err := checkAuthScheme(p); err != nil {
		return "", err
	}
	host := AuthHost(p)
	if host == "" {
		return "", fmt.Errorf("%s: no workspace host (set host or base_url)", p.Name)
	}
	return host, nil
}

// LoginU2M runs the browser login for a u2m row, seals the refresh token, mints once and returns the token's lifetime.
func LoginU2M(ctx context.Context, p *Provider, opts oauth.LoginOptions) (time.Duration, error) {
	host, err := loginHost(p)
	if err != nil {
		return 0, err
	}
	tok, err := oauth.LoginU2M(ctx, host, opts)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", p.Name, err)
	}
	if tok.Refresh == "" {
		return 0, fmt.Errorf("%s: login returned no refresh token", p.Name)
	}
	return sealLogin(p, tok)
}

// sealLogin seals a fresh login's tokens and returns the access token's lifetime.
func sealLogin(p *Provider, tok oauth.Token) (time.Duration, error) {
	// A fresh login supersedes the lock holder's refresh, so a lock timeout still seals it.
	seal := func() error {
		if err := checkIssuer(p, tok.Access); err != nil { // a wrong-issuer login must not replace the stored tokens
			return err
		}
		if err := writeOAuth(p, oauthValue{Refresh: tok.Refresh, Access: tok.Access}); err != nil {
			return fmt.Errorf("%s: seal refresh token: %w", p.Name, err)
		}
		unsealedRefresh.Delete(p.Name)
		return finishCredential(p, tok.Access, tok.Expiry)
	}
	err := withRefreshLock(p, seal)
	if errors.Is(err, errRefreshLockTimeout) {
		err = seal()
	}
	if err != nil {
		return 0, err
	}
	exp := tok.Expiry
	if e, ok := JWTExp(tok.Access); exp.IsZero() && ok {
		exp = time.Unix(e, 0)
	}
	if exp.IsZero() {
		return 0, nil
	}
	return time.Until(exp), nil
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
