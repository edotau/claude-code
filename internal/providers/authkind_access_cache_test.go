package providers

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/databricks/sdkport"
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
	if err := SealSecret(AuthSecretName(p), "sp-secret"); err != nil {
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

func TestU2MSealFailureStillReturnsMintedToken(t *testing.T) {
	p, hits, minted := u2mMint(t, "ac-sealfail")
	prev := sealer
	t.Cleanup(func() { sealer = prev })
	RegisterSealer(func(name, value string) error {
		if name == AuthSecretName(p) {
			return context.DeadlineExceeded
		}
		return prev(name, value)
	})
	got, err := Credential(context.Background(), p)
	if err != nil || got != minted || hits.Load() != 1 {
		t.Fatalf("Credential = %q, %v, hits=%d; want the minted token despite seal failure", got, err, hits.Load())
	}
	if v := sealedAccessValue(p); v != "" {
		t.Errorf("sealed access = %q, want none", v)
	}
}

func TestLoginU2MSealsAccessToken(t *testing.T) {
	idp := &rotatingIdP{}
	srv := oidcServer(t, idp.handle)
	p := mintRow(t, "ac-login", AuthOAuth, "", srv.URL)
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
		cb := "http://127.0.0.1:" + strconv.Itoa(port) + "/?code=the-code&state=" + url.QueryEscape(u.Query().Get("state"))
		go func() {
			if resp, err := http.Get(cb); err == nil {
				_ = resp.Body.Close()
			}
		}()
		return nil
	}
	if _, err := LoginU2M(context.Background(), p, sdkport.LoginOptions{Open: open, Port: port, Timeout: 20 * time.Second}); err != nil {
		t.Fatal(err)
	}
	if v := sealedAccessValue(p); v != "login-access" {
		t.Errorf("sealed access = %q, want login-access", v)
	}
}

// sealFor seals secret the way p's method stores it: u2m as the refresh of the oauth value, the rest under their name.
func sealFor(t *testing.T, p *Provider, secret string) {
	t.Helper()
	if p.Auth.Type == AuthOAuth {
		putOAuth(t, p, oauthValue{Refresh: secret})
		return
	}
	if err := SealSecret(AuthSecretName(p), secret); err != nil {
		t.Fatal(err)
	}
}
