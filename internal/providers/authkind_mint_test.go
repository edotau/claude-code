package providers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

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

func TestAuthOAuthM2MMintsViaOIDC(t *testing.T) {
	var user, pass, grant string
	var posts atomic.Int32
	srv := oidcServer(t, func(r *http.Request, f url.Values) (int, map[string]any) {
		user, pass, _ = r.BasicAuth()
		grant = f.Get("grant_type")
		posts.Add(1)
		return 200, map[string]any{"access_token": "m2m-minted", "expires_in": 3600}
	})
	p := mintRow(t, "kind-m2m-ok", AuthM2M, "sp-client", srv.URL)
	if err := SealSecret(AuthSecretName(p), "sp-secret-xyz"); err != nil {
		t.Fatal(err)
	}
	got, err := Credential(context.Background(), p)
	if err != nil || got != "m2m-minted" {
		t.Fatalf("Credential = %q, %v", got, err)
	}
	if user != "sp-client" || pass != "sp-secret-xyz" || grant != "client_credentials" {
		t.Errorf("basic %q/%q grant %q", user, pass, grant)
	}
	if _, err := Credential(context.Background(), p); err != nil || posts.Load() != 1 {
		t.Errorf("second call should hit the cache: posts=%d err=%v", posts.Load(), err)
	}
	if _, err := Refresh(context.Background(), p); err != nil || posts.Load() != 2 {
		t.Errorf("Refresh should re-mint: posts=%d err=%v", posts.Load(), err)
	}
}

func TestAuthOAuthM2MRejectedHidesSecret(t *testing.T) {
	srv := oidcServer(t, func(*http.Request, url.Values) (int, map[string]any) {
		return 401, map[string]any{"error": "invalid_client", "error_description": "Client authentication failed"}
	})
	p := mintRow(t, "kind-m2m-bad", AuthM2M, "sp-client", srv.URL)
	if err := SealSecret(AuthSecretName(p), "sp-secret-xyz"); err != nil {
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
	if err == nil || !strings.Contains(err.Error(), "databricks login --profile kind-u2m-dead") || strings.Contains(err.Error(), "refresh-dead-value") {
		t.Fatalf("err = %v", err)
	}
}

func TestKindMissingSecretErrors(t *testing.T) {
	for _, kind := range []string{AuthOAuth, AuthM2M, AuthPAT} {
		p := mintRow(t, "kind-missing-"+kind, kind, map[bool]string{true: "cid"}[kind == AuthM2M], "http://127.0.0.1:1")
		_, err := Credential(context.Background(), p)
		if err == nil || !strings.Contains(err.Error(), "no credential") {
			t.Errorf("%s: err = %v", kind, err)
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

func TestU2MEmptyRefreshNeedsLogin(t *testing.T) {
	p := mintRow(t, "kind-u2m-empty", AuthOAuth, "", "http://127.0.0.1:1")
	_, err := Credential(context.Background(), p)
	if !errors.Is(err, ErrNeedsLogin) || !strings.Contains(err.Error(), "databricks login --profile kind-u2m-empty") {
		t.Fatalf("err = %v", err)
	}
}

func TestU2MRejectedGrantNeedsLogin(t *testing.T) {
	srv := oidcServer(t, func(*http.Request, url.Values) (int, map[string]any) {
		return 400, map[string]any{"error": "invalid_grant"}
	})
	p := mintRow(t, "kind-u2m-rej", AuthOAuth, "", srv.URL)
	putOAuth(t, p, oauthValue{Refresh: "rt"})
	_, err := Credential(context.Background(), p)
	if !errors.Is(err, ErrNeedsLogin) || !strings.Contains(err.Error(), "databricks login --profile kind-u2m-rej") {
		t.Fatalf("err = %v", err)
	}
}

func TestM2MEmptySecretIsNotNeedsLogin(t *testing.T) {
	p := mintRow(t, "kind-m2m-empty", AuthM2M, "cid", "http://127.0.0.1:1")
	if _, err := Credential(context.Background(), p); err == nil || errors.Is(err, ErrNeedsLogin) {
		t.Fatalf("err = %v", err)
	}
}
