package providers

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/databricks/oauth"
	"github.com/edotau/claude-code/internal/proc"
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

func TestU2MSealedAccess(t *testing.T) {
	cases := []struct {
		name   string
		age    time.Duration
		iss    string
		force  bool
		reMint bool
	}{
		{"fresh hit makes no network call", time.Hour, "https://x", false, false},
		{"expired re-mints and re-seals", -time.Hour, "https://x", false, true},
		{"near-expiry re-mints and re-seals", expiryMargin / 2, "https://x", false, true},
		{"force bypasses a fresh sealed access", time.Hour, "https://old", true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, hits, minted := u2mMint(t, "ac-"+strings.ReplaceAll(c.name, " ", "-"))
			sealed := jwtExpIn(c.age, c.iss)
			putAccess(t, p, sealed)
			cmdCache.Delete(p.Name)
			call, want, wantHits := Credential, sealed, int32(0)
			if c.force {
				call = Refresh
			}
			if c.reMint {
				want, wantHits = minted, 1
			}
			got, err := call(context.Background(), p)
			if err != nil || got != want || hits.Load() != wantHits {
				t.Fatalf("got %q, %v, hits=%d; want %q and %d hits", got, err, hits.Load(), want, wantHits)
			}
			if v := sealedAccessValue(p); v != want {
				t.Errorf("sealed access = %q, want %q", v, want)
			}
		})
	}
}

func TestOAuthM2MNeverWritesAccessToken(t *testing.T) {
	srv := oidcServer(t, func(*http.Request, url.Values) (int, map[string]any) {
		return 200, map[string]any{"access_token": jwtExpIn(time.Hour, "https://x"), "expires_in": 3600}
	})
	p := mintRow(t, "ac-m2m", AuthM2M, "sp-client", srv.URL)
	sealFor(t, p, "sp-secret")
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
	sealFor(t, rig.p, "sp-secret-xyz")
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
	sealFor(t, p, "sp-secret-xyz")
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
	file := filepath.Join(t.TempDir(), "tok")
	if err := os.WriteFile(file, []byte("  from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, env, want, errHas string
		a                       Auth
	}{
		{"auth.file", "", "from-file", "", Auth{File: file}},
		{"auth.command", "", "minted-locked", "", Auth{Command: "echo minted-locked"}},
		{"nothing else", "", "", ": no credential (vault:", Auth{}},
		{"env over locked vault", "from-env", "from-env", "", Auth{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.a.Type, c.a.Env = AuthKey, "LOCKED_KEY"
			if c.env != "" {
				t.Setenv("LOCKED_KEY", c.env)
			}
			v, err := Credential(context.Background(), &Provider{Name: "lk-" + c.name, Auth: c.a})
			if v != c.want || (err == nil) != (c.errHas == "") || err != nil && !strings.Contains(err.Error(), c.errHas) {
				t.Errorf("Credential = %q, %v; want %q, err containing %q", v, err, c.want, c.errHas)
			}
		})
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
	write := func(body string) string {
		f := filepath.Join(t.TempDir(), "tok")
		if err := os.WriteFile(f, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return f
	}
	cases := []struct {
		name string
		a    Auth
		want bool
	}{
		{"sealed vault record, not opened", Auth{Type: AuthKey, Env: "PRESENT_KEY"}, true},
		{"auth.file", Auth{Type: AuthKey, Env: "ABSENT_KEY", File: write("tok\n")}, true},
		{"blank auth.file", Auth{Type: AuthKey, Env: "ABSENT_KEY", File: write("  \n")}, false},
		{"auth.command, not run", Auth{Type: AuthKey, Env: "ABSENT_KEY", Command: "exit 1"}, true},
		{"nothing configured", Auth{Type: AuthKey, Env: "ABSENT_KEY"}, false},
		{"none", Auth{Type: AuthNone}, true},
		{"cli", Auth{Type: AuthCLI}, true},
	}
	for _, c := range cases {
		if got := CredentialPresent(&Provider{Auth: c.a}); got != c.want {
			t.Errorf("%s: CredentialPresent = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestRunAuthCommandFailures: malformed output is refused, failures are classified, stderr secrets are masked.
func TestRunAuthCommandFailures(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := runAuthCommand(ctx, &Provider{Name: "orphan-pipe"}, `sleep 25 | cat`); err == nil {
		t.Fatal("expected a timeout error")
	}
	if d := time.Since(start); d > 12*time.Second {
		t.Fatalf("runAuthCommand blocked %v on an orphaned pipe writer", d)
	}
}

// io.Copy into a capBuffer keeps at most max bytes, however much the helper writes.
func TestCapBufferBoundsIOCopy(t *testing.T) {
	t.Parallel()
	c := &capBuffer{max: 1024}
	n, err := io.Copy(c, io.LimitReader(zeroReader{}, 1<<20))
	if err != nil || n != 1<<20 || len(c.Bytes()) != 1024 {
		t.Fatalf("copied %d (err %v), kept %d bytes; want all consumed and 1024 kept", n, err, len(c.Bytes()))
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

// countingCmd returns an auth command that appends a line to a counter file, sleeps, then runs tail.
func countingCmd(t *testing.T, name, sleep, tail string) (cmd, counter string) {
	t.Helper()
	counter = filepath.Join(t.TempDir(), name+".count")
	return fmt.Sprintf("echo x >> %q; sleep %s; %s", counter, sleep, tail), counter
}

func runCount(t *testing.T, counter string) int {
	t.Helper()
	b, err := os.ReadFile(counter)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "x\n")
}

// setFlightHook swaps the mint-flight seam for one test and restores the previous hook at cleanup.
func setFlightHook(t *testing.T, h func(leader bool)) {
	t.Helper()
	orig := flightHook
	flightHook = h
	t.Cleanup(func() { flightHook = orig })
}

func freshProvider(t *testing.T, p *Provider) *Provider {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	cmdCache.Delete(p.Name)
	t.Cleanup(func() { cmdCache.Delete(p.Name) })
	return p
}

// herd fires n concurrent Credential calls released together.
func herd(p *Provider, n int) ([]string, []error) {
	vals, errs := make([]string, n), make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			vals[i], errs[i] = Credential(context.Background(), p)
		}(i)
	}
	close(start)
	wg.Wait()
	return vals, errs
}

func TestCommandCredentialSingleFlight(t *testing.T) {
	cases := []struct{ name, tail, want, errHas string }{
		{"value", "echo shared-token", "shared-token", ""},
		{"shares error", "exit 3", "", "auth command failed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmd, counter := countingCmd(t, "sf", "0.2", c.tail)
			p := freshProvider(t, &Provider{Name: "sf-" + c.name, Auth: Auth{Type: AuthKey, Command: cmd}})
			vals, errs := herd(p, 20)
			for i := range vals {
				if vals[i] != c.want || (errs[i] == nil) != (c.errHas == "") || errs[i] != nil && !strings.Contains(errs[i].Error(), c.errHas) {
					t.Fatalf("caller %d: %q, %v; want %q, err containing %q", i, vals[i], errs[i], c.want, c.errHas)
				}
			}
			if n := runCount(t, counter); n != 1 {
				t.Fatalf("auth command ran %d times, want 1", n)
			}
			if _, ok := cmdCache.Load(p.Name); ok && c.errHas != "" {
				t.Fatal("a failed mint must not be cached")
			}
		})
	}
}

func TestRefreshDuringInflightMintRunsForcedMint(t *testing.T) {
	cmd, counter := countingCmd(t, "rf", "0.5", "echo stale-token")
	forced := counter + ".forced"
	refresh := fmt.Sprintf("echo forced >> %q; echo fresh-token", forced)
	p := freshProvider(t, &Provider{Name: "sf-refresh",
		Auth: Auth{Type: AuthKey, Command: cmd, RefreshCommand: refresh}})
	var startOnce sync.Once
	started := make(chan struct{})
	setFlightHook(t, func(leader bool) {
		if leader {
			startOnce.Do(func() { close(started) })
		}
	})
	done := make(chan struct{})
	go func() { _, _ = Credential(context.Background(), p); close(done) }()
	select {
	case <-started: // the non-forced flight is registered, its mint is running
	case <-time.After(5 * time.Second):
		t.Fatal("non-forced mint never started")
	}
	got, err := Refresh(context.Background(), p)
	if err != nil || got != "fresh-token" {
		t.Fatalf("Refresh = %q, %v; want fresh-token (forced mint, not the in-flight result)", got, err)
	}
	if b, _ := os.ReadFile(forced); strings.Count(string(b), "forced") != 1 {
		t.Fatalf("refresh command ran %q, want once", b)
	}
	<-done
}

// subprocessStartSignal is emitted by a real child of the auth shell, not the flight hook.
func subprocessStartSignal(t *testing.T) (signal struct {
	command string
	ready   chan struct{}
}) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	signal.ready = make(chan struct{})
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		_ = conn.Close()
		close(signal.ready)
	}()
	address := strings.ReplaceAll(listener.Addr().String(), ":", "/")
	signal.command = fmt.Sprintf("bash -c 'echo started > /dev/tcp/%s'", address)
	return signal
}

func TestCommandCredentialLeaderCancelDoesNotFailFollowers(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "gate")
	started := subprocessStartSignal(t)
	cmd, counter := countingCmd(t, "lc", "0", fmt.Sprintf("%s; while [ ! -e %q ]; do sleep 0.02; done; echo survived-token", started.command, gate))
	p := freshProvider(t, &Provider{Name: "sf-cancel", Auth: Auth{Type: AuthKey, Command: cmd}})
	lctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _, _ = Credential(lctx, p) }() // leader
	waitFlightSignal(t, started.ready)
	vals, errs := make([]string, 5), make([]error, 5)
	var wg sync.WaitGroup
	for i := range vals {
		wg.Add(1)
		go func(i int) { defer wg.Done(); vals[i], errs[i] = Credential(context.Background(), p) }(i)
	}
	cancel() // leader's ctx dies mid-mint: the command is blocked on the gate and has already logged its start
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	for i := range vals {
		if errs[i] != nil || vals[i] != "survived-token" {
			t.Fatalf("follower %d: %q, %v", i, vals[i], errs[i])
		}
	}
	if c := runCount(t, counter); c != 1 {
		t.Fatalf("auth command ran %d times, want 1", c)
	}
}

func TestMintOncePanicDoesNotWedgeLaterCallers(t *testing.T) {
	p := &Provider{Name: "flight-panic"}
	func() {
		defer func() { _ = recover() }()
		_, _ = mintOnce(context.Background(), p, true, func(context.Context) (string, error) { panic("boom") })
	}()
	done := make(chan string, 1)
	go func() {
		v, _ := mintOnce(context.Background(), p, true, func(context.Context) (string, error) { return "ok", nil })
		done <- v
	}()
	select {
	case v := <-done:
		if v != "ok" {
			t.Errorf("got %q", v)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("second mintOnce hung after a panicking mint")
	}
}

func TestCrossCallerAgreeEnvWinsButGetPrefersVault(t *testing.T) {
	vaulttest.UseKeyring(t)
	vaulttest.Secret(t, "CROSS_NAME", "vault-value")
	t.Setenv("CROSS_NAME", "env-value")
	p := &Provider{
		Name: "x", Protocol: ProtocolAnthropic,
		Auth: Auth{Type: AuthKey, Header: HeaderXAPIKey, Env: "CROSS_NAME"},
	}
	cv, err := Credential(context.Background(), p)
	if err != nil || cv != "env-value" {
		t.Fatalf("Credential = (%q, %v), want (env-value, nil)", cv, err)
	}
	if st := Resolve(p); st.Source != "env" {
		t.Fatalf("Resolve(p).Source = %q, want env", st.Source)
	}
}

// A timed-out auth command takes its whole pipeline with it: a backgrounded child must not outlive the call.
func TestRunAuthCommandTimeoutKillsDescendants(t *testing.T) {
	skipOffUnix(t)
	old := authCommandTimeout
	authCommandTimeout = 300 * time.Millisecond
	t.Cleanup(func() { authCommandTimeout = old })
	pidfile := filepath.Join(t.TempDir(), "child.pid")
	_, err := runAuthCommand(context.Background(), &Provider{Name: "slow"}, "sleep 60 & echo $! > "+pidfile+"; sleep 60")
	if err == nil {
		t.Fatal("a command that outlives the timeout must fail")
	}
	raw, rerr := os.ReadFile(pidfile)
	if rerr != nil {
		t.Fatalf("the descendant never started: %v", rerr)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	t.Cleanup(func() { killPID(pid) })
	for range 40 {
		if gone(t, pid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("descendant %d still running after the auth command timed out", pid)
}

// A shell that exits before its background child must not leave the child running.
func TestRunAuthCommandReapsChildAfterShellExits(t *testing.T) {
	skipOffUnix(t)
	pidfile := filepath.Join(t.TempDir(), "child.pid")
	v, err := runAuthCommand(context.Background(), &Provider{Name: "early"},
		"(sleep 60 >/dev/null 2>&1 & echo $! > "+pidfile+"); echo tok")
	if err != nil || v != "tok" {
		t.Fatalf("runAuthCommand = %q, %v; want tok", v, err)
	}
	raw, rerr := os.ReadFile(pidfile)
	if rerr != nil {
		t.Fatalf("the child never started: %v", rerr)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	t.Cleanup(func() { killPID(pid) })
	for range 40 {
		if gone(t, pid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("child %d still running after its shell exited", pid)
}

// skipOffUnix skips auth-command process tests off unix: they run sh pipelines in a process group.
func skipOffUnix(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("auth commands run under sh in a POSIX process group")
	}
}

// killPID kills a leftover test child, if it is still there.
func killPID(pid int) {
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}

// gone reports whether pid has exited; a zombie counts (a slow-reaping init must not fail the test), and an
// inspection failure fails the test rather than passing it.
func gone(t *testing.T, pid int) bool {
	t.Helper()
	if !proc.Alive(pid) {
		return true
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	st := strings.TrimSpace(string(out))
	if err != nil && st == "" {
		if !proc.Alive(pid) {
			return true
		}
		if _, ok := errors.AsType[*exec.ExitError](err); !ok {
			t.Fatalf("cannot inspect pid %d: %v", pid, err)
		}
		return false
	}
	return strings.HasPrefix(st, "Z")
}

// A background child that keeps the shell's stdout open must not delay or fail a token the shell already printed.
func TestRunAuthCommandChildHoldingStdoutDoesNotBlock(t *testing.T) {
	skipOffUnix(t)
	start := time.Now()
	v, err := runAuthCommand(context.Background(), &Provider{Name: "held"}, "sleep 60 & echo tok")
	if err != nil || v != "tok" {
		t.Fatalf("runAuthCommand = %q, %v; want tok", v, err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("took %s: a child holding stdout open must not wait out WaitDelay", d)
	}
}

// A child that escapes the group with setsid and keeps stdout open must not hang the auth command.
func TestRunAuthCommandEscapedChildDoesNotHang(t *testing.T) {
	skipOffUnix(t)
	setsid, err := exec.LookPath("setsid")
	if err != nil {
		t.Skip("setsid not on PATH")
	}
	old := authDrainLimit
	authDrainLimit = 300 * time.Millisecond
	t.Cleanup(func() { authDrainLimit = old })
	pidfile := filepath.Join(t.TempDir(), "escaped.pid")
	start := time.Now()
	v, rerr := runAuthCommand(context.Background(), &Provider{Name: "escaped"},
		setsid+" sh -c 'echo $$ > "+pidfile+"; exec sleep 60' & while [ ! -s "+pidfile+" ]; do sleep 0.05; done; echo tok")
	if raw, err := os.ReadFile(pidfile); err == nil {
		pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
		t.Cleanup(func() { killPID(pid) })
	}
	if rerr != nil || v != "tok" {
		t.Fatalf("runAuthCommand = %q, %v; want tok", v, rerr)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("took %s: draining must be bounded by authDrainLimit", d)
	}
}

// Oversized stdout is rejected, not truncated into something that passes token validation.
func TestRunAuthCommandRejectsOversizedStdout(t *testing.T) {
	skipOffUnix(t)
	_, err := runAuthCommand(context.Background(), &Provider{Name: "big"}, "head -c 1100000 /dev/zero | tr '\\0' a")
	if err == nil || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("err = %v, want the size-limit error for one token over 1 MiB", err)
	}
}

// The stderr tail survives verbose output, so the final network error still classifies the failure.
func TestRunAuthCommandKeepsStderrTail(t *testing.T) {
	skipOffUnix(t)
	_, err := runAuthCommand(context.Background(), &Provider{Name: "noisy"}, "head -c 100000 /dev/zero | tr '\\0' x >&2; echo >&2; echo 'dial tcp: connection refused' >&2; exit 1")
	if err == nil || !strings.Contains(err.Error(), "network") {
		t.Fatalf("err = %v, want a network classification from the stderr tail", err)
	}
}
