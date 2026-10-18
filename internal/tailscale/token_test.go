package tailscale

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// TestOAuthTokenMintsAndCachesUntilNearExpiry: one mint per 2s token, reused for well under that
// window, and the request body is form-encoded client_id/client_secret.
func TestOAuthTokenMintsAndCachesUntilNearExpiry(t *testing.T) {
	mints := 0
	var gotForm url.Values
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mints++
		_ = r.ParseForm()
		gotForm = r.Form
		if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
			t.Errorf("Content-Type = %q", ct)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"minted-1","expires_in":3600}`))
	}))
	defer ts.Close()

	fn := oauthToken(ts.URL, "client-id", "client-secret", ts.Client())
	tok, err := fn(context.Background())
	if err != nil || tok != "minted-1" {
		t.Fatalf("fn() = (%q, %v), want (minted-1, nil)", tok, err)
	}
	if gotForm.Get("client_id") != "client-id" || gotForm.Get("client_secret") != "client-secret" {
		t.Errorf("form = %v, want client_id/client_secret set", gotForm)
	}
	// Second call within the cache window must not hit the server again.
	if tok2, err := fn(context.Background()); err != nil || tok2 != "minted-1" || mints != 1 {
		t.Errorf("fn() (cached) = (%q, %v), mints=%d, want cached minted-1 with 1 mint", tok2, err, mints)
	}
}

// TestOAuthTokenForceRefreshBypassesCache: withForceRefresh re-mints even though the cached token
// is still fresh — the path `request` takes after a 401.
func TestOAuthTokenForceRefreshBypassesCache(t *testing.T) {
	mints := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mints++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"minted","expires_in":3600}`))
	}))
	defer ts.Close()

	fn := oauthToken(ts.URL, "id", "secret", ts.Client())
	if _, err := fn(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := fn(withForceRefresh(context.Background())); err != nil {
		t.Fatal(err)
	}
	if mints != 2 {
		t.Errorf("mints = %d, want 2 (forced refresh ignores the cache)", mints)
	}
}

// TestOAuthTokenMintFailureSurfaces: a non-200 and an empty access_token both error.
func TestOAuthTokenMintFailureSurfaces(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer ts.Close()
	if _, err := oauthToken(ts.URL, "id", "secret", ts.Client())(context.Background()); err == nil {
		t.Error("want an error on HTTP 403")
	}

	ts2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"","expires_in":3600}`))
	}))
	defer ts2.Close()
	if _, err := oauthToken(ts2.URL, "id", "secret", ts2.Client())(context.Background()); err == nil {
		t.Error("want an error on an empty access_token")
	}
}
