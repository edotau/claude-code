package providers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/databricks/sdkport"
)

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
	defer func(d time.Duration) { sdkport.RetryBackoff = d }(sdkport.RetryBackoff)
	sdkport.RetryBackoff = time.Millisecond
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
