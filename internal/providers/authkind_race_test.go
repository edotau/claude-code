package providers

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/databricks/oauth"
)

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
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent call failed: %v", err)
		}
	}

	idp.mu.Lock()
	defer idp.mu.Unlock()
	if idp.reused != 0 {
		t.Errorf("server saw %d reused refresh tokens: %v", idp.reused, idp.presented)
	}
	seen := map[string]bool{}
	for _, tok := range idp.presented {
		if seen[tok] {
			t.Errorf("refresh token %q presented twice", tok)
		}
		seen[tok] = true
	}
	if len(idp.presented) == 0 || len(idp.presented) >= n {
		t.Errorf("refresh calls = %d, want serialized (1..%d)", len(idp.presented), n-1)
	}
	if ov, err := readOAuth(p); err != nil || ov.Refresh != idp.valid {
		t.Errorf("sealed token = %q, %v; want last issued %q", ov.Refresh, err, idp.valid)
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

	open := func(authURL string) error {
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
	d, err := LoginU2M(context.Background(), p, oauth.LoginOptions{Open: open, Port: port, Timeout: 20 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if d <= 0 {
		t.Errorf("lifetime = %v, want > 0", d)
	}
	if idp.codes.Load() != 1 || idp.refreshes.Load() != 0 {
		t.Errorf("code exchanges=%d refreshes=%d, want 1 and 0", idp.codes.Load(), idp.refreshes.Load())
	}
	if v, ok := cachedCommand(p.Name); !ok || v != "login-access" {
		t.Errorf("cached = %q, %v; want login-access", v, ok)
	}
	if ov, _ := readOAuth(p); ov.Refresh != idp.valid {
		t.Errorf("sealed = %q, want %q", ov.Refresh, idp.valid)
	}
}
