package providers

import (
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
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

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
	if err := SealSecret(AuthSecretName(p), "dapi-test-value"); err != nil {
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
		{"u2m bare 403 is rejected", AuthOAuth, 403, map[string]any{}, VerdictRejected},
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
