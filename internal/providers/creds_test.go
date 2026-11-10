package providers

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/databricks/oauth"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// u2mMint is a u2m row on a token server that counts hits and mints a fresh JWT each time.
func u2mMint(t *testing.T, name string) (*Provider, *atomic.Int32, string) {
	t.Helper()
	var hits atomic.Int32
	minted := jwtExpIn(time.Hour, "https://x")
	srv := oidcServer(t, func(*http.Request, url.Values) (int, map[string]any) {
		hits.Add(1)
		return 200, map[string]any{"access_token": minted, "expires_in": 3600}
	})
	p := mintRow(t, name, AuthOAuth, "", srv.URL)
	putOAuth(t, p, oauthValue{Refresh: "refresh-1"})
	return p, &hits, minted
}

func sealedAccessValue(p *Provider) string {
	ov, _ := readOAuth(p)
	return ov.Access
}

// putOAuth seals ov as p's one oauth value; putAccess sets only its access token, keeping the refresh.
func putOAuth(t *testing.T, p *Provider, ov oauthValue) {
	t.Helper()
	if err := writeOAuth(p, ov); err != nil {
		t.Fatal(err)
	}
}

func putAccess(t *testing.T, p *Provider, tok string) {
	t.Helper()
	ov, _ := readOAuth(p)
	ov.Access = tok
	putOAuth(t, p, ov)
}

func TestU2MSealedAccessHitMakesNoNetworkCall(t *testing.T) {
	p, hits, _ := u2mMint(t, "ac-hit")
	cached := jwtExpIn(time.Hour, "https://x")
	putAccess(t, p, cached)
	cmdCache.Delete(p.Name)
	got, err := Credential(context.Background(), p)
	if err != nil || got != cached {
		t.Fatalf("Credential = %q, %v; want the sealed token", got, err)
	}
	if hits.Load() != 0 {
		t.Errorf("token server hit %d times, want 0", hits.Load())
	}
}

func TestU2MExpiredOrNearExpiryCacheReMintsAndReSeals(t *testing.T) {
	for name, d := range map[string]time.Duration{"expired": -time.Hour, "near-expiry": expiryMargin / 2} {
		t.Run(name, func(t *testing.T) {
			p, hits, minted := u2mMint(t, "ac-stale-"+name)
			putAccess(t, p, jwtExpIn(d, "https://x"))
			cmdCache.Delete(p.Name)
			got, err := Credential(context.Background(), p)
			if err != nil || got != minted {
				t.Fatalf("Credential = %q, %v; want the re-minted token", got, err)
			}
			if hits.Load() != 1 {
				t.Errorf("token server hit %d times, want 1", hits.Load())
			}
			if v := sealedAccessValue(p); v != minted {
				t.Errorf("sealed access = %q, want the re-minted token", v)
			}
		})
	}
}

func TestU2MForceBypassesSealedAccess(t *testing.T) {
	p, hits, minted := u2mMint(t, "ac-force")
	putAccess(t, p, jwtExpIn(time.Hour, "https://old"))
	cmdCache.Delete(p.Name)
	got, err := Refresh(context.Background(), p)
	if err != nil || got != minted || hits.Load() != 1 {
		t.Fatalf("Refresh = %q, %v, hits=%d; want minted token and 1 hit", got, err, hits.Load())
	}
	if v := sealedAccessValue(p); v != minted {
		t.Errorf("sealed access = %q, want the forced mint re-sealed", v)
	}
}

func TestOAuthM2MNeverWritesAccessToken(t *testing.T) {
	srv := oidcServer(t, func(*http.Request, url.Values) (int, map[string]any) {
		return 200, map[string]any{"access_token": jwtExpIn(time.Hour, "https://x"), "expires_in": 3600}
	})
	p := mintRow(t, "ac-m2m", AuthM2M, "sp-client", srv.URL)
	if err := vault.Store(AuthSecretName(p), "sp-secret"); err != nil {
		t.Fatal(err)
	}
	if _, err := Credential(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if _, err := Refresh(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if v := sealedAccessValue(p); v != "" {
		t.Errorf("m2m wrote %s = %q", AuthSecretName(p), v)
	}
}

// sealFor seals secret the way p's method stores it: u2m as the refresh of the oauth value, the rest under their name.
func sealFor(t *testing.T, p *Provider, secret string) {
	t.Helper()
	if p.Auth.Type == AuthOAuth {
		putOAuth(t, p, oauthValue{Refresh: secret})
		return
	}
	if err := vault.Store(AuthSecretName(p), secret); err != nil {
		t.Fatal(err)
	}
}

// mintRow is a non-preset row whose workspace origin (authHost) is the httptest OIDC server.
func mintRow(t *testing.T, name, kind, clientID, serverURL string) *Provider {
	t.Helper()
	vaulttest.UseKeyring(t)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	cmdCache.Delete(name)
	return &Provider{Name: name, Provider: GroupDatabricks, Protocol: ProtocolAnthropic, BaseURL: serverURL + "/serving-endpoints/anthropic",
		Auth: Auth{Type: kind, ClientID: clientID, TTL: 1800}}
}

func oidcServer(t *testing.T, h func(r *http.Request, form url.Values) (status int, body map[string]any)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		status, body := h(r, r.PostForm)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// m2mRig is an m2m row on an OIDC server that records the last basic-auth pair and grant and counts posts.
type m2mRig struct {
	user, pass, grant string
	posts             atomic.Int32
	p                 *Provider
}

func newM2MRig(t *testing.T) *m2mRig {
	rig := &m2mRig{}
	srv := oidcServer(t, func(r *http.Request, f url.Values) (int, map[string]any) {
		rig.user, rig.pass, _ = r.BasicAuth()
		rig.grant = f.Get("grant_type")
		rig.posts.Add(1)
		return 200, map[string]any{"access_token": "m2m-minted", "expires_in": 3600}
	})
	rig.p = mintRow(t, "kind-m2m-ok", AuthM2M, "sp-client", srv.URL)
	if err := vault.Store(AuthSecretName(rig.p), "sp-secret-xyz"); err != nil {
		t.Fatal(err)
	}
	return rig
}

func (r *m2mRig) mint(t *testing.T) {
	got, err := Credential(context.Background(), r.p)
	if err != nil || got != "m2m-minted" {
		t.Fatalf("Credential = %q, %v", got, err)
	}
	if r.user != "sp-client" || r.pass != "sp-secret-xyz" || r.grant != "client_credentials" {
		t.Errorf("basic %q/%q grant %q", r.user, r.pass, r.grant)
	}
}

func (r *m2mRig) cache(t *testing.T) {
	if _, err := Credential(context.Background(), r.p); err != nil || r.posts.Load() != 1 {
		t.Errorf("second call should hit the cache: posts=%d err=%v", r.posts.Load(), err)
	}
}

func (r *m2mRig) refresh(t *testing.T) {
	if _, err := Refresh(context.Background(), r.p); err != nil || r.posts.Load() != 2 {
		t.Errorf("Refresh should re-mint: posts=%d err=%v", r.posts.Load(), err)
	}
}

func TestAuthOAuthM2MMintsViaOIDC(t *testing.T) {
	rig := newM2MRig(t)
	t.Run("mint", rig.mint)
	t.Run("cache", rig.cache)
	t.Run("refresh", rig.refresh)
}

func TestAuthOAuthM2MRejectedHidesSecret(t *testing.T) {
	srv := oidcServer(t, func(*http.Request, url.Values) (int, map[string]any) {
		return 401, map[string]any{"error": "invalid_client", "error_description": "Client authentication failed"}
	})
	p := mintRow(t, "kind-m2m-bad", AuthM2M, "sp-client", srv.URL)
	if err := vault.Store(AuthSecretName(p), "sp-secret-xyz"); err != nil {
		t.Fatal(err)
	}
	_, err := Credential(context.Background(), p)
	if err == nil || !strings.Contains(err.Error(), "invalid_client") || strings.Contains(err.Error(), "sp-secret-xyz") {
		t.Fatalf("err = %v", err)
	}
}

func TestAuthU2MRotatedRefreshIsResealed(t *testing.T) {
	var seen []string
	srv := oidcServer(t, func(_ *http.Request, f url.Values) (int, map[string]any) {
		seen = append(seen, f.Get("refresh_token"))
		return 200, map[string]any{"access_token": "u2m-acc", "refresh_token": "refresh-2", "expires_in": 3600}
	})
	p := mintRow(t, "kind-u2m-rot", AuthOAuth, "", srv.URL)
	putOAuth(t, p, oauthValue{Refresh: "refresh-1"})
	if got, err := Credential(context.Background(), p); err != nil || got != "u2m-acc" {
		t.Fatalf("Credential = %q, %v", got, err)
	}
	if ov, err := readOAuth(p); err != nil || ov.Refresh != "refresh-2" {
		t.Errorf("sealed refresh after rotation = %q, %v; want refresh-2", ov.Refresh, err)
	}
	if _, err := Refresh(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0] != "refresh-1" || seen[1] != "refresh-2" {
		t.Errorf("refresh tokens presented = %v, want [refresh-1 refresh-2]", seen)
	}
}

func TestAuthU2MNonRotationKeepsSealedToken(t *testing.T) {
	srv := oidcServer(t, func(*http.Request, url.Values) (int, map[string]any) {
		return 200, map[string]any{"access_token": "u2m-acc", "expires_in": 3600}
	})
	p := mintRow(t, "kind-u2m-norot", AuthOAuth, "", srv.URL)
	putOAuth(t, p, oauthValue{Refresh: "refresh-1"})
	if _, err := Credential(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if ov, _ := readOAuth(p); ov.Refresh != "refresh-1" {
		t.Errorf("sealed refresh = %q, want unchanged", ov.Refresh)
	}
}

func TestAuthU2MInvalidGrantPointsToLoginWithoutToken(t *testing.T) {
	srv := oidcServer(t, func(*http.Request, url.Values) (int, map[string]any) {
		return 400, map[string]any{"error": "invalid_grant", "error_description": "refresh token expired"}
	})
	p := mintRow(t, "kind-u2m-dead", AuthOAuth, "", srv.URL)
	putOAuth(t, p, oauthValue{Refresh: "refresh-dead-value"})
	_, err := Credential(context.Background(), p)
	if !errors.Is(err, ErrNeedsLogin) || !strings.Contains(err.Error(), "databricks login --profile kind-u2m-dead") ||
		strings.Contains(err.Error(), "refresh-dead-value") {
		t.Fatalf("err = %v", err)
	}
}

// An unsealed row errors "no credential"; only u2m (no refresh token) is ErrNeedsLogin and names the login command.
func TestKindMissingSecretErrors(t *testing.T) {
	for _, kind := range []string{AuthOAuth, AuthM2M, AuthPAT} {
		p := mintRow(t, "kind-missing-"+kind, kind, map[bool]string{true: "cid"}[kind == AuthM2M], "http://127.0.0.1:1")
		_, err := Credential(context.Background(), p)
		u2m := kind == AuthOAuth
		if err == nil || !strings.Contains(err.Error(), "no credential") || errors.Is(err, ErrNeedsLogin) != u2m ||
			u2m && !strings.Contains(err.Error(), "databricks login --profile "+p.Name) {
			t.Errorf("%s: err = %v, want needs-login %v", kind, err, u2m)
		}
	}
}

func TestCredentialPresentPerKind(t *testing.T) {
	for _, kind := range []string{AuthOAuth, AuthM2M, AuthPAT} {
		p := mintRow(t, "kind-present-"+kind, kind, map[bool]string{true: "cid"}[kind == AuthM2M], "http://127.0.0.1:1")
		if CredentialPresent(p) {
			t.Errorf("%s: present before sealing", kind)
		}
		sealFor(t, p, "some-sealed-value")
		if !CredentialPresent(p) {
			t.Errorf("%s: absent after sealing", kind)
		}
	}
}

func TestEmptyKindKeepsCommandPath(t *testing.T) {
	p := mintRow(t, "kind-empty", "", "", "http://127.0.0.1:1")
	p.Auth.Command = "echo cmd-token"
	got, err := Credential(context.Background(), p)
	if err != nil || got != "cmd-token" {
		t.Fatalf("Credential = %q, %v", got, err)
	}
	if !CredentialPresent(p) {
		t.Error("command row not present")
	}
}

func TestKindChangeGuardCoversEmptyToKindAndForce(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	vaulttest.UseKeyring(t)
	if err := SetProviderFields("guard-row", map[string]any{
		"protocol": "anthropic", "base_url": "https://example.cloud.databricks.com/x", "auth": map[string]any{"type": "key", "command": "true"},
	}); err != nil {
		t.Fatal(err)
	}
	setKind := func(force bool) error {
		_, _, err := UpdateOverlayDocForce(true, force, func(top map[string]json.RawMessage, _ *vault.Tx) error {
			var provs map[string]map[string]any
			_ = json.Unmarshal(top["providers"], &provs)
			provs["guard-row"]["auth"] = map[string]any{"type": "pat"}
			b, _ := json.Marshal(provs)
			top["providers"] = b
			return nil
		})
		return err
	}
	if err := setKind(false); err == nil || !strings.Contains(err.Error(), "refused without force") {
		t.Fatalf("\"\"->pat without force: %v", err)
	}
	if err := setKind(true); err != nil {
		t.Fatalf("forced kind change: %v", err)
	}
}

func TestAuthU2MIgnoresStaleEnvRefresh(t *testing.T) {
	var seen string
	srv := oidcServer(t, func(_ *http.Request, f url.Values) (int, map[string]any) {
		seen = f.Get("refresh_token")
		return 200, map[string]any{"access_token": "u2m-acc", "expires_in": 3600}
	})
	p := mintRow(t, "kind-u2m-env", AuthOAuth, "", srv.URL)
	name := AuthSecretName(p)
	putOAuth(t, p, oauthValue{Refresh: "vault-refresh"})
	t.Setenv(name, "stale-env-refresh")
	if _, err := Credential(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if seen != "vault-refresh" {
		t.Errorf("token endpoint got refresh token of length %d, want the vault value", len(seen))
	}
}

// rotatingIdP is a strict OIDC token endpoint: each refresh token works once, a reused one is invalid_grant.
type rotatingIdP struct {
	mu        sync.Mutex
	valid     string
	gen       int
	presented []string
	reused    int
	refreshes atomic.Int32
	codes     atomic.Int32
}

func (s *rotatingIdP) rotate() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gen++
	s.valid = fmt.Sprintf("refresh-%d", s.gen)
	return s.valid
}

func (s *rotatingIdP) handle(_ *http.Request, f url.Values) (int, map[string]any) {
	if f.Get("grant_type") == "authorization_code" {
		s.codes.Add(1)
		return 200, map[string]any{"access_token": "login-access", "refresh_token": s.rotate(), "expires_in": 3600}
	}
	s.refreshes.Add(1)
	time.Sleep(5 * time.Millisecond) // widen the race window
	s.mu.Lock()
	tok := f.Get("refresh_token")
	s.presented = append(s.presented, tok)
	if tok != s.valid {
		s.reused++
		s.mu.Unlock()
		return 400, map[string]any{"error": "invalid_grant", "error_description": "refresh token already used"}
	}
	s.gen++
	s.valid = fmt.Sprintf("refresh-%d", s.gen)
	next, access := s.valid, fmt.Sprintf("access-%d", s.gen)
	s.mu.Unlock()
	return 200, map[string]any{"access_token": access, "refresh_token": next, "expires_in": 3600}
}

func TestRefreshRotationConcurrentNoReuse(t *testing.T) {
	idp := &rotatingIdP{}
	srv := oidcServer(t, idp.handle)
	p := mintRow(t, "race-u2m", AuthOAuth, "", srv.URL)
	putOAuth(t, p, oauthValue{Refresh: idp.rotate()})

	const n = 16
	for err := range concurrentRefreshes(p, n) {
		if err != nil {
			t.Errorf("concurrent call failed: %v", err)
		}
	}

	idp.mu.Lock()
	defer idp.mu.Unlock()
	if idp.reused != 0 {
		t.Errorf("server saw %d reused refresh tokens: %v", idp.reused, idp.presented)
	}
	checkUniqueRefreshTokens(t, idp.presented)
	if len(idp.presented) == 0 || len(idp.presented) >= n {
		t.Errorf("refresh calls = %d, want serialized (1..%d)", len(idp.presented), n-1)
	}
	if ov, err := readOAuth(p); err != nil || ov.Refresh != idp.valid {
		t.Errorf("sealed token = %q, %v; want last issued %q", ov.Refresh, err, idp.valid)
	}
}

func checkUniqueRefreshTokens(t *testing.T, tokens []string) {
	t.Helper()
	seen := map[string]bool{}
	for _, tok := range tokens {
		if seen[tok] {
			t.Errorf("refresh token %q presented twice", tok)
		}
		seen[tok] = true
	}
}

// concurrentRefreshes fires n calls released together (even: Refresh, odd: Credential) and returns their closed error channel.
func concurrentRefreshes(p *Provider, n int) <-chan error {
	var wg sync.WaitGroup
	errs := make(chan error, n)
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			var err error
			if i%2 == 0 {
				_, err = Refresh(context.Background(), p)
			} else {
				_, err = Credential(context.Background(), p)
			}
			errs <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	return errs
}

func TestValidU2MCannotReplayInflightRefresh(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var posts atomic.Int32
	idp := &rotatingIdP{}
	access := jwtExpIn(time.Hour, "https://x")
	srv := oidcServer(t, func(r *http.Request, f url.Values) (int, map[string]any) {
		if posts.Add(1) == 1 {
			close(entered)
			<-release
		}
		status, body := idp.handle(r, f)
		if status == 200 {
			body["access_token"] = access
		}
		return status, body
	})
	p := mintRow(t, "valid-inflight", AuthOAuth, "", srv.URL)
	putOAuth(t, p, oauthValue{Refresh: idp.rotate()})
	done, valid := make(chan error, 1), make(chan bool, 1)
	go func() { _, err := Credential(context.Background(), p); done <- err }()
	<-entered
	go func() { _, ok := ValidU2M(context.Background(), p); valid <- ok }()
	time.AfterFunc(100*time.Millisecond, func() { close(release) })
	err, ok := <-done, <-valid
	idp.mu.Lock()
	defer idp.mu.Unlock()
	if err != nil || !ok || idp.reused != 0 || posts.Load() != 1 {
		t.Fatalf("refresh error=%v valid=%v replays=%d posts=%d; want one successful refresh", err, ok, idp.reused, posts.Load())
	}
}

func TestRefreshRereadsTokenRotatedByAnotherProcess(t *testing.T) {
	idp := &rotatingIdP{}
	srv := oidcServer(t, idp.handle)
	p := mintRow(t, "race-xproc", AuthOAuth, "", srv.URL)
	putOAuth(t, p, oauthValue{Refresh: idp.rotate()})
	if _, err := Refresh(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	// Another process refreshes: the server rotates and that process seals the new token in the shared vault.
	other := idp.rotate()
	putOAuth(t, p, oauthValue{Refresh: other})
	if _, err := Refresh(context.Background(), p); err != nil {
		t.Fatalf("refresh after external rotation: %v", err)
	}
	idp.mu.Lock()
	defer idp.mu.Unlock()
	if idp.reused != 0 {
		t.Errorf("stale token presented: %v", idp.presented)
	}
	if last := idp.presented[len(idp.presented)-1]; last != other {
		t.Errorf("last presented %q, want the externally sealed %q", last, other)
	}
	if ov, _ := readOAuth(p); ov.Refresh != idp.valid {
		t.Errorf("sealed = %q, want %q", ov.Refresh, idp.valid)
	}
}

func TestLoginU2MDoesNotRefreshAfterExchange(t *testing.T) {
	idp := &rotatingIdP{}
	srv := oidcServer(t, idp.handle)
	p := mintRow(t, "race-login", AuthOAuth, "", srv.URL)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	d, err := LoginU2M(context.Background(), p, oauth.LoginOptions{Open: callbackOpener(port), Port: port, Timeout: 20 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if d <= 0 {
		t.Errorf("lifetime = %v, want > 0", d)
	}
	if idp.codes.Load() != 1 || idp.refreshes.Load() != 0 {
		t.Errorf("code exchanges=%d refreshes=%d, want 1 and 0", idp.codes.Load(), idp.refreshes.Load())
	}
	if v, ok := cachedCommand(p.Name); !ok || v != "login-access" || sealedAccessValue(p) != "login-access" {
		t.Errorf("cached = %q, %v, sealed %q; want login-access in both", v, ok, sealedAccessValue(p))
	}
	if ov, _ := readOAuth(p); ov.Refresh != idp.valid {
		t.Errorf("sealed = %q, want %q", ov.Refresh, idp.valid)
	}
}

// callbackOpener stands in for the browser: it GETs the loopback callback with the code and the login's own state.
func callbackOpener(port int) func(string) error {
	return func(authURL string) error {
		u, err := url.Parse(authURL)
		if err != nil {
			return err
		}
		q := u.Query()
		cb := "http://127.0.0.1:" + strconv.Itoa(port) + "/?code=the-code&state=" + url.QueryEscape(q.Get("state"))
		go func() {
			resp, err := http.Get(cb)
			if err == nil {
				_ = resp.Body.Close()
			}
		}()
		return nil
	}
}

// lockVault seeds name in the vault, then swaps HARNESS_VAULT_KEY for a different key with an empty
// keyring behind it, so the record exists but nothing can open it.
func lockVault(t *testing.T, name, value string) {
	t.Helper()
	vaulttest.Use(t)
	vaulttest.Keyring(t)
	vaulttest.Secret(t, name, value)
	if v, _, err := vault.EffectiveSecret(name); err != nil || v != value {
		t.Fatalf("precondition: vault.EffectiveSecret(%s) = %q, %v; want %q", name, v, err, value)
	}
	t.Setenv("HARNESS_VAULT_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)))
	if _, _, err := vault.EffectiveSecret(name); err == nil {
		t.Fatalf("precondition: vault.EffectiveSecret(%s) under the wrong key must error", name)
	}
}

// R14: a vault that holds the name but cannot open it must not block auth.file or auth.command.
func TestCredentialLockedVaultFallsThrough(t *testing.T) {
	lockVault(t, "LOCKED_KEY", "sealed-value")
	t.Setenv("LOCKED_KEY", "")
	t.Run("auth.file", lockedVaultAuthFile)
	t.Run("auth.command", lockedVaultAuthCommand)
	t.Run("nothing else", lockedVaultNothingElse)
	t.Run("env over locked vault", lockedVaultEnvWins)
}

func lockedVaultAuthFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "tok")
	if err := os.WriteFile(file, []byte("  from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := &Provider{Name: "lk", Auth: Auth{Type: AuthKey, Env: "LOCKED_KEY", File: file}}
	if v, err := Credential(context.Background(), p); err != nil || v != "from-file" {
		t.Errorf("Credential (locked vault, auth.file) = %q, %v; want from-file", v, err)
	}
}

func lockedVaultAuthCommand(t *testing.T) {
	p := &Provider{Name: "lk-cmd", Auth: Auth{Type: AuthKey, Env: "LOCKED_KEY", Command: "echo minted-locked"}}
	if v, err := Credential(context.Background(), p); err != nil || v != "minted-locked" {
		t.Errorf("Credential (locked vault, auth.command) = %q, %v; want minted-locked", v, err)
	}
}

func lockedVaultNothingElse(t *testing.T) {
	p := &Provider{Name: "lk-none", Auth: Auth{Type: AuthKey, Env: "LOCKED_KEY"}}
	v, err := Credential(context.Background(), p)
	if err == nil || v != "" || !strings.Contains(err.Error(), "lk-none: no credential (vault:") {
		t.Errorf("Credential (locked vault, nothing else) = %q, %v; want a \"no credential (vault: …)\" error", v, err)
	}
}

func lockedVaultEnvWins(t *testing.T) {
	p := &Provider{Name: "lk-none", Auth: Auth{Type: AuthKey, Env: "LOCKED_KEY"}}
	t.Setenv("LOCKED_KEY", "from-env")
	if v, err := Credential(context.Background(), p); err != nil || v != "from-env" {
		t.Errorf("Credential (env over a locked vault) = %q, %v; want from-env", v, err)
	}
}

// A provider with nothing configured anywhere gets the set-it hint, not a vault error.
func TestCredentialMissHint(t *testing.T) {
	vaulttest.UseKeyring(t)
	t.Setenv("MISS_KEY", "")
	p := &Provider{Name: "miss", Auth: Auth{Type: AuthKey, Env: "MISS_KEY", File: "/nonexistent/tok"}}
	_, err := Credential(context.Background(), p)
	if err == nil || strings.Contains(err.Error(), "vault:") ||
		!strings.Contains(err.Error(), "set MISS_KEY with `claude-code vault set`, or /nonexistent/tok") {
		t.Errorf("Credential(miss) err = %v; want the secrets-set hint naming MISS_KEY and auth.file", err)
	}
}

// CredentialPresent never decrypts: a sealed record, auth.file and auth.command all count under a wrong key.
func TestCredentialPresentWithoutDecrypting(t *testing.T) {
	lockVault(t, "PRESENT_KEY", "sealed-value")
	t.Setenv("PRESENT_KEY", "")
	t.Setenv("ABSENT_KEY", "")

	if !CredentialPresent(&Provider{Auth: Auth{Type: AuthKey, Env: "PRESENT_KEY"}}) {
		t.Error("CredentialPresent must report a sealed vault record without opening it")
	}
	file := filepath.Join(t.TempDir(), "tok")
	if err := os.WriteFile(file, []byte("tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !CredentialPresent(&Provider{Auth: Auth{Type: AuthKey, Env: "ABSENT_KEY", File: file}}) {
		t.Error("CredentialPresent must keep auth.file as a source")
	}
	blank := filepath.Join(t.TempDir(), "blank")
	if err := os.WriteFile(blank, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if CredentialPresent(&Provider{Auth: Auth{Type: AuthKey, Env: "ABSENT_KEY", File: blank}}) {
		t.Error("CredentialPresent must not count a blank auth.file")
	}
	if !CredentialPresent(&Provider{Auth: Auth{Type: AuthKey, Env: "ABSENT_KEY", Command: "exit 1"}}) {
		t.Error("CredentialPresent must keep auth.command as a source (without running it)")
	}
	if CredentialPresent(&Provider{Auth: Auth{Type: AuthKey, Env: "ABSENT_KEY"}}) {
		t.Error("CredentialPresent with nothing configured must be false")
	}
	checkCredentialFreeTypesPresent(t)
}

func checkCredentialFreeTypesPresent(t *testing.T) {
	t.Helper()
	for _, typ := range []string{AuthNone, AuthCLI} {
		if !CredentialPresent(&Provider{Auth: Auth{Type: typ}}) {
			t.Errorf("CredentialPresent(%s) must be true", typ)
		}
	}
}

// TestRunAuthCommandFailures: malformed output is refused, failures are classified, stderr secrets are masked.
func TestRunAuthCommandFailures(t *testing.T) {
	p := &Provider{Name: "row"}
	cases := []struct{ cmd, want, absent string }{
		{`echo null`, "printed nothing", ""},
		{`printf 'tok1\ntok2\n'`, "more than one bare token", "tok1"},
		{`echo "dial tcp: lookup x: no such host, token eyJabc.def.ghi" >&2; exit 1`, "(network:", "eyJabc"},
		{`echo "invalid refresh token: dapiSECRET" >&2; exit 1`, "(auth:", "dapiSECRET"},
		{`definitely-not-a-command-xyz`, "(cli: command not found)", ""},
	}
	for _, c := range cases {
		_, err := runAuthCommand(context.Background(), p, c.cmd)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v; want it to contain %q", c.cmd, err, c.want)
		}
		if c.absent != "" && err != nil && strings.Contains(err.Error(), c.absent) {
			t.Errorf("%s: err leaks %q: %v", c.cmd, c.absent, err)
		}
	}
	if v, err := runAuthCommand(context.Background(), p, `echo '  tok  '`); err != nil || v != "tok" {
		t.Errorf("good token = %q, %v; want tok, nil", v, err)
	}
}

func TestRunAuthCommandTimeoutNotHeldByOrphanedPipe(t *testing.T) {
	p := &Provider{Name: "orphan-pipe"}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := runAuthCommand(ctx, p, `sleep 25 | cat`)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if d := time.Since(start); d > 12*time.Second {
		t.Fatalf("runAuthCommand blocked %v on an orphaned pipe writer", d)
	}
}
