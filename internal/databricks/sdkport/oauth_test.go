package sdkport

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// freePort returns a currently free loopback port.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// drive returns an Open func that performs the browser callback with the given state transform.
func drive(t *testing.T, code string, state func(string) string, errc chan<- error) func(string) error {
	return func(authURL string) error {
		go func() {
			u, err := url.Parse(authURL)
			if err != nil {
				errc <- err
				return
			}
			q := u.Query()
			cb := q.Get("redirect_uri") + "/?code=" + url.QueryEscape(code) + "&state=" + url.QueryEscape(state(q.Get("state")))
			resp, err := http.Get(cb)
			if err != nil {
				errc <- err
				return
			}
			resp.Body.Close()
			errc <- nil
		}()
		return nil
	}
}

func same(s string) string { return s }

func tokenJSON(w http.ResponseWriter, access, refresh string) {
	w.Header().Set("Content-Type", "application/json")
	m := map[string]any{"access_token": access, "expires_in": 3600, "token_type": "Bearer"}
	if refresh != "" {
		m["refresh_token"] = refresh
	}
	_ = json.NewEncoder(w).Encode(m)
}

func TestLoginU2MSuccessAndPKCE(t *testing.T) {
	var gotForm url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotForm = r.PostForm
		tokenJSON(w, "acc-1", "ref-1")
	}))
	defer srv.Close()

	var authURL string
	errc := make(chan error, 1)
	inner := drive(t, "the-code", same, errc)
	open := func(a string) error { authURL = a; return inner(a) }
	tok, err := LoginU2M(context.Background(), srv.URL, LoginOptions{Open: open, Port: freePort(t), Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if cerr := <-errc; cerr != nil {
		t.Fatal(cerr)
	}
	if tok.Access != "acc-1" || tok.Refresh != "ref-1" || tok.Expiry.IsZero() {
		t.Errorf("token = %+v", tok)
	}
	aq, _ := url.Parse(authURL)
	q := aq.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("client_id") != ClientID || q.Get("scope") != "all-apis offline_access" {
		t.Errorf("authorize query = %v", q)
	}
	if !strings.HasPrefix(q.Get("redirect_uri"), "http://localhost:") {
		t.Errorf("redirect = %q", q.Get("redirect_uri"))
	}
	// Server-side PKCE: challenge == base64url(sha256(verifier)).
	verifier := gotForm.Get("code_verifier")
	sum := sha256.Sum256([]byte(verifier))
	if want := base64.RawURLEncoding.EncodeToString(sum[:]); q.Get("code_challenge") != want || verifier == "" {
		t.Errorf("challenge %q != S256(verifier) %q", q.Get("code_challenge"), want)
	}
	if raw, _ := base64.RawURLEncoding.DecodeString(verifier); len(raw) != 32 {
		t.Errorf("verifier decodes to %d bytes, want 32", len(raw))
	}
	if gotForm.Get("grant_type") != "authorization_code" || gotForm.Get("code") != "the-code" ||
		gotForm.Get("redirect_uri") != q.Get("redirect_uri") {
		t.Errorf("exchange form = %v", gotForm)
	}
}

func TestLoginU2MWrongStateRejectedThenRightStateSucceeds(t *testing.T) {
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		tokenJSON(w, "acc", "ref")
	}))
	defer srv.Close()

	var status int
	open := func(authURL string) error {
		u, _ := url.Parse(authURL)
		q := u.Query()
		go func() {
			bad, err := http.Get(q.Get("redirect_uri") + "/?code=evil&state=wrong")
			if err == nil {
				status = bad.StatusCode
				bad.Body.Close()
			}
			if ok, err := http.Get(q.Get("redirect_uri") + "/?code=good&state=" + url.QueryEscape(q.Get("state"))); err == nil {
				ok.Body.Close()
			}
		}()
		return nil
	}
	tok, err := LoginU2M(context.Background(), srv.URL, LoginOptions{Open: open, Port: freePort(t), Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusBadRequest {
		t.Errorf("wrong-state status = %d, want 400", status)
	}
	if tok.Access != "acc" || posts.Load() != 1 {
		t.Errorf("tok=%+v posts=%d; wrong state must not trigger an exchange", tok, posts.Load())
	}
}

func TestLoginU2MWrongStateOnlyTimesOutWithoutExchange(t *testing.T) {
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { posts.Add(1); tokenJSON(w, "a", "r") }))
	defer srv.Close()
	errc := make(chan error, 1)
	_, err := LoginU2M(context.Background(), srv.URL, LoginOptions{
		Open: drive(t, "evil", func(string) string { return "forged" }, errc), Port: freePort(t), Timeout: 700 * time.Millisecond,
	})
	if err == nil || posts.Load() != 0 {
		t.Fatalf("err=%v posts=%d, want timeout and no token POST", err, posts.Load())
	}
	if strings.Contains(err.Error(), "evil") {
		t.Errorf("error leaks code: %v", err)
	}
}

func TestLoginU2MTokenErrorSurfacesDescriptionNotCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"invalid_request","error_description":"redirect_uri not registered"}`)
	}))
	defer srv.Close()
	errc := make(chan error, 1)
	_, err := LoginU2M(context.Background(), srv.URL, LoginOptions{Open: drive(t, "SECRET-CODE-123", same, errc), Port: freePort(t), Timeout: 10 * time.Second})
	if err == nil || !strings.Contains(err.Error(), "redirect_uri not registered") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "SECRET-CODE-123") {
		t.Errorf("error leaks the authorization code: %v", err)
	}
}

func TestLoginU2MNoRefreshTokenIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { tokenJSON(w, "acc-only", "") }))
	defer srv.Close()
	errc := make(chan error, 1)
	_, err := LoginU2M(context.Background(), srv.URL, LoginOptions{Open: drive(t, "c", same, errc), Port: freePort(t), Timeout: 10 * time.Second})
	if err == nil || !strings.Contains(err.Error(), "no refresh token") || strings.Contains(err.Error(), "acc-only") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoginU2MHeadlessPrintsURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { tokenJSON(w, "a", "r") }))
	defer srv.Close()
	var out strings.Builder
	errc := make(chan error, 1)
	inner := drive(t, "c", same, errc)
	// Open fails after starting the callback: URL must go to Out.
	open := func(a string) error { _ = inner(a); return fmt.Errorf("no browser") }
	if _, err := LoginU2M(context.Background(), srv.URL, LoginOptions{Open: open, Out: &out, Port: freePort(t), Timeout: 10 * time.Second}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "/oidc/v1/authorize?") {
		t.Errorf("Out = %q", out.String())
	}
}

func TestCallbackBindsLoopbackOnly(t *testing.T) {
	ln, _, err := listenLoopback(freePort(t))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if ip := ln.Addr().(*net.TCPAddr).IP; !ip.IsLoopback() {
		t.Errorf("listener bound to %v", ip)
	}
}

func TestRefreshU2MRotationAndNonRotation(t *testing.T) {
	rotate := true
	var form url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form = r.PostForm
		if rotate {
			tokenJSON(w, "acc2", "ref-new")
			return
		}
		tokenJSON(w, "acc3", "")
	}))
	defer srv.Close()
	tok, err := RefreshU2M(context.Background(), srv.URL, "ref-old")
	if err != nil || tok.Access != "acc2" || tok.Refresh != "ref-new" {
		t.Fatalf("rotated: %+v %v", tok, err)
	}
	if form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != "ref-old" || form.Get("client_id") != ClientID {
		t.Errorf("form = %v", form)
	}
	rotate = false
	tok, err = RefreshU2M(context.Background(), srv.URL, "ref-old")
	if err != nil || tok.Refresh != "ref-old" {
		t.Fatalf("non-rotated: %+v %v", tok, err)
	}
	if _, err := RefreshU2M(context.Background(), srv.URL, ""); err == nil {
		t.Error("empty refresh token accepted")
	}
}

func TestMintM2MSuccess(t *testing.T) {
	var user, pass string
	var form url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, _ = r.BasicAuth()
		_ = r.ParseForm()
		form = r.PostForm
		tokenJSON(w, "m2m-acc", "should-be-dropped")
	}))
	defer srv.Close()
	tok, err := MintM2M(context.Background(), srv.URL+"/", "sp-id", "sp-secret")
	if err != nil {
		t.Fatal(err)
	}
	if tok.Access != "m2m-acc" || tok.Refresh != "" || tok.Expiry.IsZero() {
		t.Errorf("token = %+v", tok)
	}
	if user != "sp-id" || pass != "sp-secret" || form.Get("grant_type") != "client_credentials" || form.Get("scope") != "all-apis" {
		t.Errorf("basic=%q/%q form=%v", user, pass, form)
	}
}

func TestMintM2MInvalidClientMessageHasNoSecret(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"invalid_client","error_description":"Client authentication failed"}`)
	}))
	defer srv.Close()
	_, err := MintM2M(context.Background(), srv.URL, "sp-id", "sp-secret-value")
	if err == nil || !strings.Contains(err.Error(), "invalid_client") ||
		!strings.Contains(err.Error(), "Client authentication failed") || strings.Contains(err.Error(), "sp-secret-value") {
		t.Fatalf("err = %v", err)
	}
	if _, err := MintM2M(context.Background(), srv.URL, "", ""); err == nil {
		t.Error("empty credentials accepted")
	}
}

func TestMintM2MSerializedPerHost(t *testing.T) {
	var inflight, peak, total atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inflight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		total.Add(1)
		time.Sleep(60 * time.Millisecond)
		inflight.Add(-1)
		tokenJSON(w, "a", "")
	}))
	defer srv.Close()
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := MintM2M(context.Background(), srv.URL, "id", "s"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if peak.Load() != 1 || total.Load() != 4 {
		t.Errorf("peak concurrent POSTs = %d (want 1), total = %d (want 4)", peak.Load(), total.Load())
	}
}

func TestPostTokenRetriesOn503ThenSucceeds(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		tokenJSON(w, "after-retry", "")
	}))
	defer srv.Close()
	tok, err := MintM2M(context.Background(), srv.URL, "id", "s")
	if err != nil || tok.Access != "after-retry" || n.Load() != 2 {
		t.Fatalf("tok=%+v err=%v posts=%d", tok, err, n.Load())
	}
}

func TestPostTokenDoesNotRetry400(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()
	if _, err := RefreshU2M(context.Background(), srv.URL, "r"); err == nil || n.Load() != 1 {
		t.Fatalf("err=%v posts=%d, want one failed POST", err, n.Load())
	}
}

func TestNormHost(t *testing.T) {
	if h, err := normHost(" https://example.cloud.databricks.com/ "); err != nil || h != "https://example.cloud.databricks.com" {
		t.Errorf("%q %v", h, err)
	}
	for _, bad := range []string{"", "example.cloud.databricks.com", "ftp://x"} {
		if _, err := normHost(bad); err == nil {
			t.Errorf("normHost(%q) accepted", bad)
		}
	}
}

func TestIsLoginRequired(t *testing.T) {
	oe := func(code string, status int) error {
		return &oauthError{Host: "h", Op: "refresh", Code: code, Status: status}
	}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"invalid_grant", oe("invalid_grant", 400), true},
		{"invalid_client", oe("invalid_client", 401), true},
		{"unauthorized_client", oe("unauthorized_client", 400), true},
		{"access_denied", oe("access_denied", 400), true},
		{"invalid_scope", oe("invalid_scope", 400), true},
		{"case-sensitive", oe("Invalid_Grant", 400), false},
		{"429", oe("", 429), false},
		{"503", oe("", 503), false},
		{"500", oe("", 500), false},
		{"transport", errors.New("dial tcp: connection refused"), false},
		{"nil", nil, false},
		{"wrapped", fmt.Errorf("ctx: %w", oe("invalid_grant", 400)), true},
	}
	for _, c := range cases {
		if got := IsLoginRequired(c.err); got != c.want {
			t.Errorf("%s: IsLoginRequired = %v, want %v", c.name, got, c.want)
		}
	}
}
