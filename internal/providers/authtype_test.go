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
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/databricks/oauth"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestAuthMethodPerType(t *testing.T) {
	for _, typ := range []string{AuthOAuth, AuthM2M, AuthPAT, AuthKey, AuthCLI, AuthNone} {
		t.Run(typ, func(t *testing.T) { checkAuthType(t, typ) })
	}
	t.Run("unset", func(t *testing.T) {
		if (Auth{}).Method() != "" {
			t.Error("an unset Auth must report an empty method")
		}
	})
	t.Run("explicit header", func(t *testing.T) {
		if got := (Auth{Type: AuthKey, Header: HeaderXAPIKey}).HeaderStyle(); got != HeaderXAPIKey {
			t.Errorf("explicit header = %q", got)
		}
	})
}

func checkAuthType(t *testing.T, typ string) {
	t.Helper()
	a := Auth{Type: typ}
	if a.Method() != typ {
		t.Errorf("Method() for %q = %q", typ, a.Method())
	}
	wantSealed := slices.Contains([]string{AuthOAuth, AuthM2M, AuthPAT}, typ)
	if a.Sealed() != wantSealed {
		t.Errorf("Sealed() for %q = %v, want %v", typ, a.Sealed(), wantSealed)
	}
	wantHeader := ""
	if wantSealed || typ == AuthKey {
		wantHeader = HeaderBearer
	}
	if a.HeaderStyle() != wantHeader {
		t.Errorf("HeaderStyle() for %q = %q, want %q", typ, a.HeaderStyle(), wantHeader)
	}
}

func TestRetiredAuthSpellingsAreRejected(t *testing.T) {
	for _, typ := range []string{"bearer", "u2m", "oauth-m2m", "api-key", "passthrough"} {
		doc := `{"providers":{"anthropic":{"x":{"protocol":"anthropic","base_url":"https://x.example","auth":{"type":"` + typ + `"}}}}}`
		if _, err := parseOverlay(t, doc); err == nil || !strings.Contains(err.Error(), "unknown") {
			t.Errorf("retired auth type %q: err = %v, want an unknown-type error", typ, err)
		}
	}
}

func TestFlatOverlayIsNotReadByTheRuntime(t *testing.T) {
	flat := `{"providers":{"gw":{"protocol":"anthropic","base_url":"https://gw.example","auth":{"type":"none"}}}}`
	if _, err := parseOverlay(t, flat); err == nil || !strings.Contains(err.Error(), `unknown provider "gw"`) {
		t.Fatalf("err = %v, want the flat row refused as an unknown provider", err)
	}
}

func TestOneMIsIgnoredOnLoad(t *testing.T) {
	doc := `{"providers":{"anthropic":{"x":{"protocol":"anthropic","base_url":"https://x.example","auth":{"type":"none"},"one_m":true}}}}`
	reg, err := parseOverlay(t, doc)
	if err != nil {
		t.Fatalf("one_m must not break loading: %v", err)
	}
	if p := reg.Providers["x"]; p == nil || p.Auth.Method() != AuthNone {
		t.Errorf("row x = %+v", p)
	}
}

func TestLegacyPinNamesAreNotRead(t *testing.T) {
	reg := load(t)
	writeProviderPins(t, map[string]string{"HARNESS_PROVIDER": "dev", "HARNESS_MODEL": "legacy-model", "COPILOT_GITHUB_USER": "someone"})
	sel, err := Select(reg, "")
	if err != nil {
		t.Fatal(err)
	}
	if sel.Provider.Name == "dev" {
		t.Error("legacy HARNESS_PROVIDER pin selected a profile")
	}
	for alias, tg := range sel.Aliases {
		if tg.Model == "legacy-model" {
			t.Errorf("alias %s took the legacy HARNESS_MODEL pin", alias)
		}
	}
	if got, _ := CopilotLogin(reg); got == "someone" {
		t.Error("legacy COPILOT_GITHUB_USER pin was read as a login")
	}
}

func kindRow(kind, clientID, _ string) *Provider {
	if kind == "" {
		kind = AuthKey
	}
	return &Provider{Name: "dbx-test", Provider: GroupDatabricks, Protocol: ProtocolAnthropic, BaseURL: "https://example.cloud.databricks.com/serving-endpoints/anthropic",
		Auth: Auth{Type: kind, ClientID: clientID}}
}

// validateAuthType checks the companion fields of a sealed kind, clientId injection, and account/workspace id shapes.
func TestValidateAuthType(t *testing.T) {
	cases := []struct{ name, kind, clientID, acct, ws, want string }{
		{"none", "", "", "", "", ""},
		{"u2m", AuthOAuth, "", "", "", ""},
		{"m2m ok", AuthM2M, "sp-client", "", "", ""},
		{"m2m no client", AuthM2M, "", "", "", "needs auth.clientId"},
		{"stray clientId", AuthPAT, "cid", "", "", "needs auth.type m2m"},
		{"ids ok", "", "", "1234abcd-0000-ffff", "1234567890", ""},
		{"short account", "", "", "short", "", "auth.accountId"},
		{"non-hex account", "", "", "zzzzzzzzzz", "", "auth.accountId"},
		{"non-numeric workspace", "", "", "", "12ab", "auth.workspaceId"},
	}
	for _, id := range []string{"a\nb", "a\rb", "a[b", "a=b"} {
		cases = append(cases, struct{ name, kind, clientID, acct, ws, want string }{"inject " + id, AuthM2M, id, "", "", "auth.clientId must not contain"})
	}
	for _, c := range cases {
		p := kindRow(c.kind, c.clientID, "")
		p.Auth.AccountID, p.Auth.WorkspaceID = c.acct, c.ws
		errs := strings.Join(validateAuthType("x", p), ";")
		if c.want == "" && errs != "" || c.want != "" && !strings.Contains(errs, c.want) {
			t.Errorf("%s: errs %q, want %q", c.name, errs, c.want)
		}
	}
}

func TestKindSecretName(t *testing.T) {
	p := kindRow(AuthOAuth, "", AuthKey)
	p.Name = "dbx-dev.1"
	if got := AuthSecretName(p); got != "DATABRICKS_DBX_DEV_1_OAUTH" {
		t.Errorf("got %q", got)
	}
	if AuthSecretName(kindRow("", "", AuthKey)) != "" {
		t.Error("kindless row has a sealed name")
	}
}

func TestPATCredentialAndPresence(t *testing.T) {
	vaulttest.Use(t) // sealing needs the vault master key; Linux CI has no keyring
	p := kindRow(AuthPAT, "", AuthKey)
	if CredentialPresent(p) {
		t.Fatal("present before sealing")
	}
	if err := vault.Store(AuthSecretName(p), "dapi-test-value"); err != nil {
		t.Fatal(err)
	}
	if !CredentialPresent(p) {
		t.Error("absent after sealing")
	}
	got, err := Credential(context.Background(), p)
	if err != nil || got != "dapi-test-value" {
		t.Errorf("Credential = %q, %v", got, err)
	}
}

func TestKindChangeRefusedWithoutForce(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	row := map[string]any{"auth": map[string]any{"type": "pat"}}
	if err := SetProviderFields("openai", row); err != nil {
		t.Fatal(err)
	}
	row["auth"] = map[string]any{"type": "oauth"}
	err := SetProviderFields("openai", row)
	if err == nil || !strings.Contains(err.Error(), "refused without force") {
		t.Fatalf("kind change err = %v", err)
	}
	if err := SetProviderFields("openai", map[string]any{"base_url": "https://a.example/v1"}); err != nil {
		t.Errorf("same-kind write refused: %v", err)
	}
}

// CredentialNames covers the sealed PAT/M2M/OAuth secret name too, so an isolating scrub drops it from other children.
func TestCredentialNamesIncludeSealedSecret(t *testing.T) {
	for _, kind := range []string{AuthPAT, AuthM2M, AuthOAuth} {
		p := kindRow(kind, "", AuthKey)
		want := AuthSecretName(p)
		if want == "" || !slices.Contains(CredentialNames(p), want) {
			t.Errorf("%s: CredentialNames = %v, want it to include %q", kind, CredentialNames(p), want)
		}
	}
}

func TestValidIDShape(t *testing.T) {
	for id, want := range map[string]bool{"my-sp_1": true, "": false, "bad name": false, "a/b": false} {
		if ValidID(id) != want {
			t.Errorf("ValidID(%q) = %v, want %v", id, !want, want)
		}
	}
}

// An OAuth provider's missing-credential hint names both the databricks login and the configure Set up route.
func TestAuthHintOAuthNamesLoginAndSetUp(t *testing.T) {
	p := kindRow(AuthOAuth, "", "")
	p.Name = "dbx"
	got := authHint(p, "DBX_TOKEN")
	for _, want := range []string{"claude-code databricks login --profile dbx", "claude-code configure", "Set up"} {
		if !strings.Contains(got, want) {
			t.Errorf("OAuth hint %q lacks %q", got, want)
		}
	}
	if key := authHint(kindRow(AuthKey, "", ""), "DBX_TOKEN"); !strings.Contains(key, "vault set") || strings.Contains(key, "databricks login") {
		t.Errorf("key-auth hint = %q, want the vault set route only", key)
	}
}

// mintErr mints p against an OIDC stub answering status/body and returns the error Credential surfaces.
func mintErr(t *testing.T, kind string, status int, body map[string]any) error {
	t.Helper()
	srv := oidcServer(t, func(*http.Request, url.Values) (int, map[string]any) { return status, body })
	p := mintRow(t, fmt.Sprintf("verdict-%s-%d", kind, status), kind, "sp-client", srv.URL)
	sealFor(t, p, "sealed-secret")
	_, err := Credential(context.Background(), p)
	if err == nil {
		t.Fatalf("status %d: mint unexpectedly succeeded", status)
	}
	return err
}

func TestVerdictFromRealMintErrors(t *testing.T) {
	defer func(d time.Duration) { oauth.RetryBackoff = d }(oauth.RetryBackoff)
	oauth.RetryBackoff = time.Millisecond
	cases := []struct {
		name   string
		kind   string
		status int
		body   map[string]any
		want   AuthVerdict
	}{
		{"u2m invalid_grant needs login", AuthOAuth, 400, map[string]any{"error": "invalid_grant"}, VerdictNeedsLogin},
		{"m2m invalid_client needs login", AuthM2M, 400, map[string]any{"error": "invalid_client"}, VerdictNeedsLogin},
		{"u2m bare 401 is rejected", AuthOAuth, 401, map[string]any{}, VerdictRejected},
		{"u2m bare 403 is transient", AuthOAuth, 403, map[string]any{}, VerdictTransient},
		{"u2m 503 is transient", AuthOAuth, 503, map[string]any{}, VerdictTransient},
		{"u2m 429 is transient", AuthOAuth, 429, map[string]any{"error": "rate_limited"}, VerdictTransient},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := Verdict(mintErr(t, c.kind, c.status, c.body))
			if v != c.want {
				t.Fatalf("Verdict = %q, want %q", v, c.want)
			}
			if v.Condemns() != (c.want != VerdictTransient) {
				t.Fatalf("Condemns() = %v for %q", v.Condemns(), v)
			}
		})
	}
}

func TestVerdictU2MWithoutSealedTokenNeedsLogin(t *testing.T) {
	p := mintRow(t, "verdict-u2m-empty", AuthOAuth, "", "https://example.cloud.databricks.com")
	_, err := Credential(context.Background(), p)
	if got := Verdict(err); got != VerdictNeedsLogin || !got.Condemns() {
		t.Fatalf("Verdict(%v) = %q, want needs_login", err, got)
	}
	if got := Verdict(fmt.Errorf("wrapped: %w", err)); got != VerdictNeedsLogin {
		t.Fatalf("a wrapped login error lost its verdict: %q", got)
	}
}

// Transport failures and timeouts say nothing about the credential: they must never condemn the account.
func TestVerdictTransientNeverCondemns(t *testing.T) {
	deadline, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-deadline.Done()
	for name, err := range map[string]error{
		"deadline exceeded":     context.DeadlineExceeded,
		"wrapped deadline":      fmt.Errorf("mint: %w", deadline.Err()),
		"canceled":              context.Canceled,
		"transport":             &url.Error{Op: "Post", URL: "https://x", Err: errors.New("connection refused")},
		"plain":                 errors.New("boom"),
		"auth.command failed":   errors.New("exit status 1"),
		"vault read":            errors.New("vault: locked"),
		"scheme refused (http)": errors.New("oauth: host uses scheme http"),
	} {
		if v := Verdict(err); v != VerdictTransient || v.Condemns() {
			t.Errorf("%s: Verdict = %q (condemns %v), want transient/false", name, v, v.Condemns())
		}
	}
	if v := Verdict(nil); v != VerdictOK || v.Condemns() || AuthVerdict("").Condemns() {
		t.Errorf("nil: %q condemns %v, or an empty verdict condemns", v, v.Condemns())
	}
}

func TestIsInvalidTokenResponse(t *testing.T) {
	invalid := []byte(`{"message":"Invalid Token"}`)
	dbx := &Provider{Name: "dbx", Preset: PresetDatabricks}
	sealed := &Provider{Name: "sealed", Auth: Auth{Type: AuthOAuth}}
	plain := &Provider{Name: "plain", Auth: Auth{Type: AuthKey, Env: "K"}}
	cases := []struct {
		name   string
		p      *Provider
		status int
		body   []byte
		want   bool
	}{
		{"databricks preset 400", dbx, 400, invalid, true},
		{"sealed-auth row 400", sealed, 400, invalid, true},
		{"non-databricks provider", plain, 400, invalid, false},
		{"nil provider", nil, 400, invalid, false},
		{"401 is not the gateway shape", dbx, 401, invalid, false},
		{"500", dbx, 500, invalid, false},
		{"other 400 body", dbx, 400, []byte(`{"message":"bad request"}`), false},
		{"empty body", dbx, 400, nil, false},
	}
	for _, c := range cases {
		if got := IsInvalidTokenResponse(c.p, c.status, c.body); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// spHarness seals a refresh token and an SP secret on a paired u2m row; refresh is rejected (401), M2M is counted.
func spHarness(t *testing.T, identity string) (p *Provider, m2mCalls, refreshCalls *int, note *bytes.Buffer) {
	t.Helper()
	srv := oidcServer(t, func(*http.Request, url.Values) (int, map[string]any) { return 401, map[string]any{} })
	p = mintRow(t, "sp-"+strings.ReplaceAll(t.Name(), "/", "-"), AuthOAuth, "", srv.URL)
	p.Identity, p.Auth.SPClientID = identity, "sp-client"
	spCache.Delete(p.Name)
	t.Cleanup(func() { spCache.Delete(p.Name) })
	if err := writeOAuth(p, oauthValue{Refresh: "user-refresh"}); err != nil {
		t.Fatal(err)
	}
	if err := vault.Store(SecretName(GroupDatabricks, p.Name, AuthM2M), "sp-secret"); err != nil {
		t.Fatal(err)
	}
	m2mCalls, refreshCalls, note = new(int), new(int), &bytes.Buffer{}
	origR, origM, origN := refreshU2M, mintM2M, spNoteOut
	refreshU2M = func(ctx context.Context, host, rt string) (oauth.Token, error) {
		*refreshCalls++
		return oauth.RefreshU2M(ctx, srv.URL, rt)
	}
	mintM2M = func(_ context.Context, _, id, secret string) (oauth.Token, error) {
		*m2mCalls++
		if id != "sp-client" || secret != "sp-secret" {
			t.Errorf("MintM2M got %q/%q", id, secret)
		}
		return oauth.Token{Access: "sp-token"}, nil
	}
	spNoteOut = note
	t.Cleanup(func() { refreshU2M, mintM2M, spNoteOut = origR, origM, origN })
	return p, m2mCalls, refreshCalls, note
}

func TestIdentityAutoFallsBackToSPOnRejection(t *testing.T) {
	p, m2m, _, note := spHarness(t, "")
	got, err := Credential(context.Background(), p)
	if err != nil || got != "sp-token" || *m2m != 1 {
		t.Fatalf("Credential = %q, %v (m2m %d)", got, err, *m2m)
	}
	if n := note.String(); !strings.Contains(n, "service principal") || strings.Contains(n, "sp-secret") || strings.Count(n, "\n") != 1 {
		t.Errorf("note = %q", n)
	}
}

// An SP mint records its expiry tagged m2m, so the statusline countdown shows on a paired oauth row.
func TestSPMintRecordsExpiryAsM2M(t *testing.T) {
	p, _, _, _ := spHarness(t, IdentitySP)
	stubMintM2M(t, jwtExpIn(time.Hour, ""))
	if _, err := Credential(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	exp, method, ok := TokenExpiryMethod(p.Name)
	if !ok || method != AuthM2M || time.Until(exp) < 50*time.Minute {
		t.Fatalf("record = %v, %q, %v; want ~1h, m2m", exp, method, ok)
	}
}

// A transient SP mint failure rides out on the sealed _ACCESS token, but never for force or under 1 minute.
func TestTransientSPMintFallsBackToSealedAccess(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name  string
		left  time.Duration
		force bool
		want  bool
	}{{"stale-ok", 5 * time.Minute, false, true}, {"force", 5 * time.Minute, true, false}, {"short", 30 * time.Second, false, false}} {
		p, _, _, _ := spHarness(t, IdentitySP)
		old := jwtExpIn(c.left, "")
		bind := clientBind(AuthHost(p), "sp-client", "sp-secret")
		b, _ := json.Marshal(clientAccess{Access: old, Bind: bind})
		if err := vault.Store(SecretName(GroupDatabricks, p.Name, AuthM2M)+"_ACCESS", string(b)); err != nil {
			t.Fatal(err)
		}
		orig := mintM2M
		mintM2M = func(context.Context, string, string, string) (oauth.Token, error) {
			return oauth.Token{}, errors.New("connection refused")
		}
		got, err := mintSP(ctx, p, c.force)
		mintM2M = orig
		if served := err == nil && got == old; served != c.want {
			t.Errorf("%s: served the sealed token = %v (err %v), want %v", c.name, served, err, c.want)
		}
	}
}

func TestIdentityUserNeverMintsSP(t *testing.T) {
	p, m2m, _, _ := spHarness(t, IdentityUser)
	_, err := Credential(context.Background(), p)
	if err == nil || !strings.Contains(err.Error(), "databricks login") || *m2m != 0 {
		t.Fatalf("err = %v, m2m calls %d", err, *m2m)
	}
}

func TestIdentitySPSkipsUserLogin(t *testing.T) {
	p, m2m, refresh, _ := spHarness(t, IdentitySP)
	got, err := Credential(context.Background(), p)
	if err != nil || got != "sp-token" || *m2m != 1 || *refresh != 0 {
		t.Fatalf("got %q, %v (m2m %d, refresh %d)", got, err, *m2m, *refresh)
	}
}

func TestIdentityHeadlessUsesSPOnAutoRowOnly(t *testing.T) {
	headless := WithIdentity(context.Background(), IdentityHeadless)
	p, m2m, refresh, _ := spHarness(t, IdentityAuto)
	if got, err := Credential(headless, p); err != nil || got != "sp-token" || *m2m != 1 || *refresh != 0 {
		t.Fatalf("headless on an auto row = %q, %v (m2m %d, refresh %d); want the SP, no user refresh", got, err, *m2m, *refresh)
	}
	u, m2m, _, _ := spHarness(t, IdentityUser)
	if _, err := Credential(headless, u); err == nil || *m2m != 0 {
		t.Fatalf("headless on a user row minted the SP: %v (m2m %d)", err, *m2m)
	}
	if got := headlessAs(IdentityHeadless); got != "" {
		t.Errorf("unpaired row headless = %q; want no override", got)
	}
}

func TestIdentityFlagBeatsProfile(t *testing.T) {
	if got, err := ResolveIdentity(IdentityUser, IdentitySP); err != nil || got != IdentitySP {
		t.Fatalf("flag sp over profile user = %q, %v", got, err)
	}
	p, m2m, refresh, _ := spHarness(t, IdentityUser)
	if got, err := Credential(WithIdentity(context.Background(), IdentitySP), p); err != nil || got != "sp-token" || *m2m != 1 || *refresh != 0 {
		t.Fatalf("--as sp on a user row = %q, %v (m2m %d)", got, err, *m2m)
	}
	if _, err := ResolveIdentity("", "bogus"); err == nil {
		t.Error("bogus --as accepted")
	}
	if got, _ := ResolveIdentity("", ""); got != IdentityAuto {
		t.Errorf("default = %q", got)
	}
}

func TestUnpairedRowJSONOmitsIdentity(t *testing.T) {
	b, err := Provider{Name: "plain", Auth: Auth{Type: AuthOAuth}}.MarshalJSON()
	if err != nil || strings.Contains(string(b), "identity") || strings.Contains(string(b), "spClientId") {
		t.Fatalf("%s, %v", b, err)
	}
}

func TestIdentitySPCacheCannotAuthenticateUser(t *testing.T) {
	p, m2m, refresh, _ := spHarness(t, IdentitySP)
	if _, err := Credential(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if _, err := Credential(WithIdentity(context.Background(), IdentityUser), p); err == nil {
		t.Fatal("user-only request accepted the cached SP credential")
	}
	other := *p
	other.Name += ".sp"
	if _, err := Credential(WithIdentity(context.Background(), IdentityUser), &other); err == nil {
		t.Fatal("another profile accepted the cached SP credential")
	}
	if *m2m != 1 || *refresh != 1 {
		t.Fatalf("m2m=%d refresh=%d; want one independent request per identity", *m2m, *refresh)
	}
}

func TestIdentityUserCannotJoinAutoSPFlight(t *testing.T) {
	p, _, _, _ := spHarness(t, IdentityAuto)
	entered, release := make(chan struct{}), make(chan struct{})
	mintM2M = func(context.Context, string, string, string) (oauth.Token, error) {
		close(entered)
		<-release
		return oauth.Token{Access: "sp-token"}, nil
	}
	done := make(chan error, 1)
	go func() { _, err := Credential(context.Background(), p); done <- err }()
	t.Cleanup(func() { close(release); <-done })
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("auto request never reached SP mint")
	}
	ctx, cancel := context.WithTimeout(WithIdentity(context.Background(), IdentityUser), time.Second)
	defer cancel()
	_, err := Credential(ctx, p)
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("user-only request joined the auto fallback flight")
	}
}

// Auto mode with no sealed user login goes straight to the paired SP: no user refresh is attempted.
func TestIdentityAutoWithoutUserLoginSkipsRefresh(t *testing.T) {
	p, m2m, refresh, _ := spHarness(t, "")
	if err := vault.Delete(AuthSecretName(p)); err != nil {
		t.Fatal(err)
	}
	got, err := Credential(context.Background(), p)
	if err != nil || got != "sp-token" || *m2m != 1 || *refresh != 0 {
		t.Fatalf("Credential = %q, %v (m2m %d, refresh %d)", got, err, *m2m, *refresh)
	}
	lock := filepath.Join(paths.StateDir(), "locks", "auth-"+lockNameRE.ReplaceAllString(p.Name, "_")+".lock")
	if _, err := os.Stat(lock); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the user refresh lock was taken: %v", err)
	}
}

// A sealed SP token is served by a new process without a mint and without the fallback note; a mint notes once.
func TestSPNoteOnlyOnMint(t *testing.T) {
	p, m2m, _, out := spHarness(t, IdentitySP)
	tok := jwtExpIn(time.Hour, "")
	mintM2M = func(context.Context, string, string, string) (oauth.Token, error) {
		*m2m++
		return oauth.Token{Access: tok}, nil
	}
	var mu sync.Mutex
	var notes []string
	ctx := WithNotes(context.Background(), func(s string) { mu.Lock(); notes = append(notes, s); mu.Unlock() })
	if v, err := Credential(ctx, p); err != nil || v != tok {
		t.Fatalf("first Credential = %v", err)
	}
	spCache.Delete(p.Name) // a new process: only the sealed _ACCESS slot remains
	if v, err := Credential(ctx, p); err != nil || v != tok {
		t.Fatalf("second Credential = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if *m2m != 1 || len(notes) != 1 || !strings.Contains(notes[0], "using the service principal sp-client") || out.Len() != 0 {
		t.Fatalf("m2m = %d, notes = %q, stderr note = %q", *m2m, notes, out.String())
	}
}

// idCreds is which credentials an identityRow stores; rejected makes the token endpoint refuse the SP (a real 401).
type idCreds struct{ sp, user, rejected bool }

// identityRow is a paired oauth row on identity with c's credentials; the user refresh and SP mint are stubbed.
func identityRow(t *testing.T, name, identity string, c idCreds) *Provider {
	t.Helper()
	srv := oidcServer(t, func(*http.Request, url.Values) (int, map[string]any) {
		return http.StatusUnauthorized, map[string]any{"error": "invalid_client"}
	})
	p := mintRow(t, name, AuthOAuth, "", srv.URL)
	p.Identity, p.Auth.SPClientID = identity, "sp-client"
	t.Cleanup(func() { spCache.Delete(p.Name); spSkip.Delete(p.Name) })
	if c.user {
		if err := writeOAuth(p, oauthValue{Refresh: "user-refresh"}); err != nil {
			t.Fatal(err)
		}
	}
	if c.sp {
		if err := vault.Store(SecretName(GroupDatabricks, p.Name, AuthM2M), "sp-secret"); err != nil {
			t.Fatal(err)
		}
	}
	origR, origM := refreshU2M, mintM2M
	refreshU2M = func(context.Context, string, string) (oauth.Token, error) { return oauth.Token{Access: "user"}, nil }
	mintM2M = func(ctx context.Context, _, id, secret string) (oauth.Token, error) {
		if c.rejected {
			return oauth.MintM2M(ctx, srv.URL, id, secret)
		}
		return oauth.Token{Access: "sp"}, nil
	}
	t.Cleanup(func() { refreshU2M, mintM2M = origR, origM })
	return p
}

// resolveCell mints once on a fresh row and returns the credential used ("" on error), " fb" when a fallback was noted.
func resolveCell(t *testing.T, ctx context.Context, name, identity string, c idCreds) string {
	t.Helper()
	p := identityRow(t, name, identity, c)
	var notes strings.Builder
	got, err := Credential(WithNotes(ctx, func(s string) { notes.WriteString(s + "\n") }), p)
	if err != nil {
		got = ""
	}
	if strings.Contains(notes.String(), "sp-secret") {
		t.Errorf("%s: a note leaked the secret: %q", name, notes.String())
	}
	if strings.Contains(notes.String(), "falling back to your own login") {
		got += " fb"
	}
	return got
}

// A dispatch on an auto row mints as the SP first and falls back to the user login with a note; an explicit identity
// never falls back, and an interactive launch on the same row keeps auto's user-first order.
func TestIdentityResolutionTable(t *testing.T) {
	modes := []struct{ name, identity, as string }{
		{"auto", IdentityAuto, ""}, {"sp", IdentitySP, ""}, {"user", IdentityUser, ""},
		{"as-sp", IdentityUser, IdentitySP}, {"as-user", IdentitySP, IdentityUser},
	}
	creds := map[string]idCreds{"sp-only": {sp: true}, "user-only": {user: true}, "both": {sp: true, user: true},
		"neither": {}, "both-rejected": {sp: true, user: true, rejected: true}}
	want := map[string]map[string][5]string{ // per path and creds: auto, sp, user, --as sp, --as user
		"dispatch": {"sp-only": {"sp", "sp", "", "sp", ""}, "user-only": {"user fb", "", "user", "", "user"},
			"both": {"sp", "sp", "user", "sp", "user"}, "neither": {" fb", "", "", "", ""},
			"both-rejected": {"user fb", "", "user", "", "user"}},
		"interactive": {"sp-only": {"sp", "sp", "", "sp", ""}, "user-only": {"user", "", "user", "", "user"},
			"both": {"user", "sp", "user", "sp", "user"}, "neither": {"", "", "", "", ""},
			"both-rejected": {"user", "", "user", "", "user"}},
	}
	n := 0
	for path, rows := range want {
		for cname, cells := range rows {
			for i, m := range modes {
				n++
				as := m.as
				if path == "dispatch" { // what agents ask, pool and job put on a run's context
					as = cmp.Or(as, IdentityHeadless)
				}
				got := resolveCell(t, WithIdentity(context.Background(), as), fmt.Sprintf("idrow-%d", n), m.identity, creds[cname])
				if got != cells[i] {
					t.Errorf("%s %s × %s = %q, want %q", path, cname, m.name, got, cells[i])
				}
			}
		}
	}
}

const dbxHost = "https://example.cloud.databricks.com"

func patRow(host string) string {
	return `{"preset":"databricks","host":"` + host + `","auth":{"type":"pat"}}`
}

// Two rows whose sealed secret names collide (a-b, a_b) are refused, naming both; distinct names pass.
func TestValidateSealedSecretNameCollision(t *testing.T) {
	for second, collides := range map[string]bool{"a_b": true, "c_d": false} {
		doc := `{"providers":{"databricks":{"a-b":` + patRow(dbxHost) + `,"` + second + `":` + patRow(dbxHost) + `}}}`
		err := Validate([]byte(doc))
		if collides != (err != nil) || collides && (!strings.Contains(err.Error(), "a-b") || !strings.Contains(err.Error(), second)) {
			t.Errorf("%s: Validate = %v, want collision %v naming both rows", second, err, collides)
		}
	}
}

func m2mBaseRow(base string) string {
	return `{"protocol":"anthropic","base_url":"` + base + `/serving-endpoints/anthropic",` +
		`"auth":{"type":"m2m","clientId":"sp-client"}}`
}

// A sealed row on plain http is refused unless the host is loopback; https always passes.
func TestValidateSealedHostScheme(t *testing.T) {
	for doc, ok := range map[string]bool{
		`{"m2m-http":` + m2mBaseRow("http://example.cloud.databricks.com") + `}`: false,
		`{"pat-http":` + patRow("http://example.cloud.databricks.com") + `}`:     false,
		`{"m2m-ok":` + m2mBaseRow(dbxHost) + `}`:                                 true,
		`{"m2m-ok":` + m2mBaseRow("http://127.0.0.1:8080") + `}`:                 true,
		`{"m2m-ok":` + m2mBaseRow("http://localhost:9") + `}`:                    true,
	} {
		if err := Validate([]byte(`{"providers":{"databricks":` + doc + `}}`)); ok != (err == nil) {
			t.Errorf("Validate(%s) = %v, want accepted %v", doc, err, ok)
		}
	}
}

func TestAuthHostRejectsPlainHTTPNonLoopback(t *testing.T) {
	p := &Provider{Name: "x", BaseURL: "http://example.cloud.databricks.com/serving-endpoints/anthropic",
		Auth: Auth{Type: AuthM2M, ClientID: "c"}}
	if h := AuthHost(p); h != "" {
		t.Errorf("AuthHost = %q, want empty for plain http non-loopback", h)
	}
}

func TestLoopbackHTTPSealedRowStillMints(t *testing.T) {
	srv := oidcServer(t, func(_ *http.Request, _ url.Values) (int, map[string]any) {
		return 200, map[string]any{"access_token": "loop-minted", "expires_in": 3600}
	})
	p := mintRow(t, "kind-loop-http", AuthM2M, "sp-client", srv.URL)
	if err := vault.Store(AuthSecretName(p), "sp-secret"); err != nil {
		t.Fatal(err)
	}
	if got, err := Credential(context.Background(), p); err != nil || got != "loop-minted" {
		t.Fatalf("Credential = %q, %v", got, err)
	}
}

func TestPlainHTTPNonLoopbackSealedRowDoesNotMint(t *testing.T) {
	p := mintRow(t, "kind-nonloop-http", AuthM2M, "sp-client", "http://example.cloud.databricks.com")
	if err := vault.Store(AuthSecretName(p), "sp-secret"); err != nil {
		t.Fatal(err)
	}
	got, err := Credential(context.Background(), p)
	if err == nil || got != "" || strings.Contains(err.Error(), "sp-secret") {
		t.Fatalf("Credential = %q, %v; want an error and no token", got, err)
	}
}

// clientRow is an m2m row, or with sp a paired u2m row on identity sp, plus the vault name of its client secret.
func clientRow(t *testing.T, sp bool) (*Provider, string) {
	if sp {
		p, _, _, _ := spHarness(t, IdentitySP)
		return p, SecretName(GroupDatabricks, p.Name, AuthM2M)
	}
	p := mintRow(t, "m2m-share", AuthM2M, "client", "https://h.example.com")
	if err := vault.Store(AuthSecretName(p), "secret"); err != nil {
		t.Fatal(err)
	}
	return p, AuthSecretName(p)
}

// An m2m/sp token minted by one process is reused by the next until its client secret changes or a refresh forces it.
func TestClientTokenSharedAcrossProcesses(t *testing.T) {
	for _, sp := range []bool{false, true} {
		t.Run(fmt.Sprint("sp=", sp), func(t *testing.T) { checkClientTokenSharing(t, sp) })
	}
}

// checkClientTokenSharing runs the reuse, rotation and forced-mint steps for one client row (m2m, or paired sp).
func checkClientTokenSharing(t *testing.T, sp bool) {
	p, secretName := clientRow(t, sp)
	calls, orig := 0, mintM2M
	mintM2M = func(context.Context, string, string, string) (oauth.Token, error) {
		calls++
		return oauth.Token{Access: jwtExpIn(time.Hour+time.Duration(calls)*time.Second, "")}, nil
	}
	t.Cleanup(func() { mintM2M = orig })
	newProcess := func() { cmdCache.Delete(p.Name); spCache.Delete(p.Name) }
	ctx := context.Background()
	first, err := Credential(ctx, p)
	newProcess()
	again, err2 := Credential(ctx, p)
	if err != nil || err2 != nil || again != first || calls != 1 {
		t.Fatalf("second process: same=%t calls=%d errs=%v,%v; want the sealed token reused", again == first, calls, err, err2)
	}
	if err := vault.Store(secretName, "rotated"); err != nil {
		t.Fatal(err)
	}
	newProcess()
	if rotated, err := Credential(ctx, p); err != nil || rotated == first || calls != 2 {
		t.Fatalf("after secret rotation: reused=%t calls=%d err=%v; want a fresh mint", rotated == first, calls, err)
	}
	checkClientForcedRefresh(t, p, &calls)
}

func checkClientForcedRefresh(t *testing.T, p *Provider, calls *int) {
	t.Helper()
	if _, err := Refresh(context.Background(), p); err != nil || *calls != 3 {
		t.Fatalf("Refresh: calls=%d err=%v; want a forced mint", *calls, err)
	}
}

// A minted token that fails the issuer check is refused before it is sealed into the shared vault.
func TestM2MWrongIssuerTokenNotSealed(t *testing.T) {
	p, secretName := clientRow(t, false)
	p.Preset, p.Host = PresetDatabricks, "https://h.example.com"
	stubMintM2M(t, jwtExpIn(time.Hour, "https://other.example.org/oidc"))
	_, err := m2mToken(context.Background(), p, secretName, "h.example.com", "client", "secret", true)
	if _, ok := errors.AsType[*HostMismatchError](err); !ok {
		t.Fatalf("m2mToken err = %v; want HostMismatchError", err)
	}
	if vaultHas(t, secretName+"_ACCESS") {
		t.Fatal("wrong-issuer token was sealed in the vault")
	}
}

// stubMintM2M makes every M2M mint return tok for this test.
func stubMintM2M(t *testing.T, tok string) {
	t.Helper()
	orig := mintM2M
	mintM2M = func(context.Context, string, string, string) (oauth.Token, error) {
		return oauth.Token{Access: tok}, nil
	}
	t.Cleanup(func() { mintM2M = orig })
}

func vaultHas(t *testing.T, name string) bool {
	t.Helper()
	_, found, err := vault.ReadSecret(name)
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// An m2m row and a paired profile cache their SP token under <PROFILE>_M2M_ACCESS, never a hash name.
func TestM2MTokenCachedUnderProfileName(t *testing.T) {
	for _, sp := range []bool{false, true} {
		t.Run(fmt.Sprint("sp=", sp), func(t *testing.T) {
			p, secretName := clientRow(t, sp)
			stubMintM2M(t, jwtExpIn(time.Hour, ""))
			if _, err := Refresh(context.Background(), p); err != nil {
				t.Fatal(err)
			}
			if !vaultHas(t, secretName+"_ACCESS") {
				t.Fatalf("no token cache at %s_ACCESS", secretName)
			}
		})
	}
}

// A mint sweeps the old hash-named DATABRICKS_M2M_<HASH>_ACCESS slot, auditing a delete only when the slot existed.
func TestM2MMintSweepsLegacyHashSlot(t *testing.T) {
	for _, present := range []bool{true, false} {
		t.Run(fmt.Sprint("present=", present), func(t *testing.T) {
			p, secretName := clientRow(t, false)
			legacySwept.Store(false) // a fresh process; an earlier test's mint may have set it
			sum := sha256.Sum256([]byte("h.example.com\x00client\x00secret"))
			legacy := "DATABRICKS_M2M_" + strings.ToUpper(hex.EncodeToString(sum[:8])) + "_ACCESS"
			if present {
				if err := vault.Store(legacy, "stale"); err != nil {
					t.Fatal(err)
				}
			}
			stubMintM2M(t, jwtExpIn(time.Hour, ""))
			since := time.Now().Add(-time.Second)
			if _, err := m2mToken(context.Background(), p, secretName, "h.example.com", "client", "secret", true); err != nil {
				t.Fatal(err)
			}
			events, err := audit.Read(since)
			if err != nil {
				t.Fatal(err)
			}
			deleted := slices.ContainsFunc(events, func(e audit.Event) bool { return e.Action == audit.SecretDelete })
			if vaultHas(t, legacy) || deleted != present {
				t.Fatalf("legacy slot left=%v, delete audited=%v; want swept, audited only when present", vaultHas(t, legacy), deleted)
			}
		})
	}
}

// A forced u2m mint sealed by one process is adopted, with no refresh call, by another whose own token is near expiry.
func TestForcedU2MMintAdoptedByOtherProcess(t *testing.T) {
	p := mintRow(t, "u2m-adopt", AuthOAuth, "", "https://h.example.com")
	if err := writeOAuth(p, oauthValue{Refresh: "rt"}); err != nil {
		t.Fatal(err)
	}
	calls, orig := 0, refreshU2M
	refreshU2M = func(context.Context, string, string) (oauth.Token, error) {
		calls++
		return oauth.Token{Access: jwtExpIn(time.Hour+time.Duration(calls)*time.Second, ""), Refresh: "rt"}, nil
	}
	t.Cleanup(func() { refreshU2M = orig })
	t.Cleanup(func() { cmdCache.Delete(p.Name) })
	ctx := context.Background()
	forced, err := Refresh(ctx, p) // process A
	if err != nil || calls != 1 {
		t.Fatalf("forced mint: calls=%d err=%v", calls, err)
	}
	cmdCache.Store(p.Name, cached{value: "b-old-token", until: time.Now().Add(-time.Second)}) // process B, token near expiry
	got, err := Credential(ctx, p)
	if err != nil || got != forced || calls != 1 {
		t.Fatalf("process B: adopted=%t calls=%d err=%v; want A's sealed token with no mint", got == forced, calls, err)
	}
}

// A forced m2m mint on one row seals only that row's slot; another row on the same principal keeps its own token.
func TestM2MForcedMintLeavesOtherRowSlot(t *testing.T) {
	p, nameA := clientRow(t, false)
	nameB := nameA + "_OTHER"
	calls, orig := 0, mintM2M
	mintM2M = func(context.Context, string, string, string) (oauth.Token, error) {
		calls++
		return oauth.Token{Access: jwtExpIn(time.Hour+time.Duration(calls)*time.Second, "")}, nil
	}
	t.Cleanup(func() { mintM2M = orig })
	ctx := context.Background()
	a1, errA := m2mToken(ctx, p, nameA, "h.example.com", "client", "secret", true)
	b1, errB := m2mToken(ctx, p, nameB, "h.example.com", "client", "secret", true)
	a2, errA2 := m2mToken(ctx, p, nameA, "h.example.com", "client", "secret", true)
	bAgain, errB2 := m2mToken(ctx, p, nameB, "h.example.com", "client", "secret", false)
	if errA != nil || errB != nil || errA2 != nil || errB2 != nil {
		t.Fatalf("errs = %v %v %v %v", errA, errB, errA2, errB2)
	}
	if a1 == b1 || a2 == a1 || bAgain != b1 || calls != 3 {
		t.Fatalf("row B reused=%t rowA rotated=%t calls=%d; want B's own sealed token and 3 mints", bAgain == b1, a2 != a1, calls)
	}
}
