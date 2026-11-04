package providers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/databricks/oauth"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestAuthMethodPerType(t *testing.T) {
	for _, typ := range []string{AuthOAuth, AuthM2M, AuthPAT, AuthKey, AuthCLI, AuthNone} {
		a := Auth{Type: typ}
		if a.Method() != typ {
			t.Errorf("Method() for %q = %q", typ, a.Method())
		}
		wantSealed := typ == AuthOAuth || typ == AuthM2M || typ == AuthPAT
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
	if (Auth{}).Method() != "" {
		t.Error("an unset Auth must report an empty method")
	}
	if got := (Auth{Type: AuthKey, Header: HeaderXAPIKey}).HeaderStyle(); got != HeaderXAPIKey {
		t.Errorf("explicit header = %q", got)
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
	reg, err := parseOverlay(t, flat)
	if err == nil {
		if reg.Providers["gw"] != nil {
			t.Fatal("flat overlay row gw was read")
		}
		return
	}
	if !strings.Contains(err.Error(), `unknown provider "gw"`) {
		t.Errorf("err = %v, want it to name the unknown provider", err)
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

func TestValidateKind(t *testing.T) {
	cases := []struct {
		name string
		p    *Provider
		want string
	}{
		{"none", kindRow("", "", AuthKey), ""},
		{"u2m", kindRow(AuthOAuth, "", AuthKey), ""},
		{"m2m ok", kindRow(AuthM2M, "cid", AuthKey), ""},
		{"m2m no client", kindRow(AuthM2M, "", AuthKey), "needs auth.clientId"},
		{"stray clientId", kindRow(AuthPAT, "cid", ""), "needs auth.type m2m"},
	}
	for _, c := range cases {
		errs := strings.Join(validateAuthType("x", c.p), ";")
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

func TestAccountAndWorkspaceIDValidation(t *testing.T) {
	cases := []struct{ acct, ws, want string }{
		{"", "", ""},
		{"1234abcd-0000-ffff", "1234567890", ""},
		{"short", "", "auth.accountId"},
		{"zzzzzzzzzz", "", "auth.accountId"},
		{"", "12ab", "auth.workspaceId"},
	}
	for _, c := range cases {
		p := kindRow("", "", AuthKey)
		p.Auth.AccountID, p.Auth.WorkspaceID = c.acct, c.ws
		errs := strings.Join(validateAuthType("x", p), ";")
		if c.want == "" && errs != "" || c.want != "" && !strings.Contains(errs, c.want) {
			t.Errorf("acct=%q ws=%q: errs %q, want %q", c.acct, c.ws, errs, c.want)
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

func TestClientIDInjectionRejected(t *testing.T) {
	for _, id := range []string{"a\nb", "a\rb", "a[b", "a=b"} {
		p := kindRow(AuthM2M, id, AuthKey)
		if errs := strings.Join(validateAuthType("x", p), ";"); !strings.Contains(errs, "auth.clientId must not contain") {
			t.Errorf("clientId %q: errs %q", id, errs)
		}
	}
	if errs := validateAuthType("x", kindRow(AuthM2M, "sp-client", AuthKey)); len(errs) != 0 {
		t.Errorf("clean clientId rejected: %v", errs)
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
	if v := Verdict(nil); v != VerdictOK || v.Condemns() {
		t.Errorf("nil: %q condemns %v", v, v.Condemns())
	}
}

func TestVerdictCondemnsOnlyLoginAndRejected(t *testing.T) {
	for v, want := range map[AuthVerdict]bool{
		VerdictOK: false, VerdictTransient: false, VerdictNeedsLogin: true, VerdictRejected: true, "": false,
	} {
		if v.Condemns() != want {
			t.Errorf("%q.Condemns() = %v, want %v", v, v.Condemns(), want)
		}
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

const dbxHost = "https://example.cloud.databricks.com"

func patRow(host string) string {
	return `{"preset":"databricks","host":"` + host + `","auth":{"type":"pat"}}`
}

func TestValidateRejectsCollidingSealedSecretNames(t *testing.T) {
	doc := `{"providers":{"databricks":{"a-b":` + patRow(dbxHost) + `,"a_b":` + patRow(dbxHost) + `}}}`
	err := Validate([]byte(doc))
	if err == nil || !strings.Contains(err.Error(), "a-b") || !strings.Contains(err.Error(), "a_b") {
		t.Fatalf("Validate = %v, want an error naming a-b and a_b", err)
	}
}

func TestValidateAcceptsDistinctSealedSecretNames(t *testing.T) {
	doc := `{"providers":{"databricks":{"a-b":` + patRow(dbxHost) + `,"c_d":` + patRow(dbxHost) + `}}}`
	if err := Validate([]byte(doc)); err != nil {
		t.Fatalf("Validate = %v", err)
	}
}

func m2mBaseRow(base string) string {
	return `{"protocol":"anthropic","base_url":"` + base + `/serving-endpoints/anthropic",` +
		`"auth":{"type":"m2m","clientId":"sp-client"}}`
}

func TestValidateRejectsPlainHTTPSealedHost(t *testing.T) {
	for _, doc := range []string{
		`{"providers":{"databricks":{"m2m-http":` + m2mBaseRow("http://example.cloud.databricks.com") + `}}}`,
		`{"providers":{"databricks":{"pat-http":{"preset":"databricks","host":"http://example.cloud.databricks.com","auth":{"type":"pat"}}}}}`,
	} {
		if err := Validate([]byte(doc)); err == nil {
			t.Errorf("Validate accepted plain http: %s", doc)
		}
	}
}

func TestValidateAcceptsHTTPSAndLoopbackHTTPSealedHost(t *testing.T) {
	for _, base := range []string{dbxHost, "http://127.0.0.1:8080", "http://localhost:9"} {
		doc := `{"providers":{"databricks":{"m2m-ok":` + m2mBaseRow(base) + `}}}`
		if err := Validate([]byte(doc)); err != nil {
			t.Errorf("Validate(%s) = %v", base, err)
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
