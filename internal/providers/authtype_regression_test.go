package providers

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/databricks/oauth"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestSealLoginWrongIssuerPreservesStoredTokens(t *testing.T) {
	vaulttest.Use(t)
	p := &Provider{Name: "login-issuer", Provider: GroupDatabricks, Preset: PresetDatabricks,
		Host: "https://expected.example.com", Auth: Auth{Type: AuthOAuth}}
	const previous = `{"refresh":"previous-refresh","access":"previous-access"}`
	vaulttest.Secret(t, AuthSecretName(p), previous)
	cmdCache.Delete(p.Name)
	t.Cleanup(func() { cmdCache.Delete(p.Name) })
	_, err := sealLogin(p, oauth.Token{Refresh: "wrong-refresh",
		Access: jwtExpIn(time.Hour, "https://wrong.example.com/oidc"), Expiry: time.Now().Add(time.Hour)})
	if _, ok := errors.AsType[*HostMismatchError](err); !ok {
		t.Fatalf("sealLogin error = %v, want HostMismatchError", err)
	}
	stored, found, readErr := vault.ReadSecret(AuthSecretName(p))
	if readErr != nil || !found || stored != previous {
		t.Fatalf("wrong issuer replaced stored tokens: found=%t unchanged=%t err=%v", found, stored == previous, readErr)
	}
	if _, found := cmdCache.Load(p.Name); found {
		t.Fatal("wrong issuer populated the command cache")
	}
}

// rotRow is a u2m row whose token endpoint rotates to refresh-2 and breaks the vault dir after the first refresh.
func rotRow(t *testing.T) (*Provider, *[]string, string) {
	t.Helper()
	origDelay := sealRetryDelay
	sealRetryDelay = time.Millisecond
	t.Cleanup(func() { sealRetryDelay = origDelay; unsealedRefresh.Delete("rot-carry") })
	broken := t.TempDir() // state is a file here, so every vault write fails
	if err := os.WriteFile(filepath.Join(broken, "state"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var seen []string
	srv := oidcServer(t, func(_ *http.Request, f url.Values) (int, map[string]any) {
		if seen = append(seen, f.Get("refresh_token")); len(seen) == 1 {
			t.Setenv("CLAUDE_CONFIG_DIR", broken)
		}
		return http.StatusOK, map[string]any{"access_token": jwtExpIn(time.Hour, "https://x"), "expires_in": 3600, "refresh_token": "refresh-2"}
	})
	p := mintRow(t, "rot-carry", AuthOAuth, "", srv.URL)
	putOAuth(t, p, oauthValue{Refresh: "refresh-1"})
	return p, &seen, os.Getenv("CLAUDE_CONFIG_DIR")
}

// A rotated refresh token whose re-seal fails stays in memory, and the next mint seals it and clears the entry.
func TestRotatedRefreshSealFailureCarriedToNextMint(t *testing.T) {
	p, seen, good := rotRow(t)
	if _, err := Refresh(context.Background(), p); err != nil {
		t.Fatalf("first mint = %v; want success despite the failed re-seal", err)
	}
	if v, ok := unsealedRefresh.Load(p.Name); !ok || v != "refresh-2" {
		t.Fatalf("unsealedRefresh = %v, %v; want the rotated refresh-2 kept in memory", v, ok)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", good)
	if _, err := Refresh(context.Background(), p); err != nil {
		t.Fatalf("second mint = %v", err)
	}
	if len(*seen) != 2 || (*seen)[1] != "refresh-2" {
		t.Errorf("refresh tokens sent = %q; want the second mint to send the in-memory refresh-2", *seen)
	}
	if ov, _ := readOAuth(p); ov.Refresh != "refresh-2" {
		t.Errorf("sealed refresh = %q; want refresh-2 sealed by the second mint", ov.Refresh)
	}
	if _, ok := unsealedRefresh.Load(p.Name); ok {
		t.Error("unsealedRefresh still holds the token after a successful seal")
	}
}

// staleRow is a u2m row on a token endpoint that always answers 503, with a sealed access token valid for left.
func staleRow(t *testing.T, name string, left time.Duration) (*Provider, string) {
	t.Helper()
	srv := oidcServer(t, func(*http.Request, url.Values) (int, map[string]any) {
		return http.StatusServiceUnavailable, map[string]any{"error": "temporarily_unavailable"}
	})
	p := mintRow(t, name, AuthOAuth, "", srv.URL)
	t.Cleanup(func() { cmdCache.Delete(name) })
	old := jwtExpIn(left, "https://x")
	putOAuth(t, p, oauthValue{Refresh: "refresh-1", Access: old})
	return p, old
}

// A transient refresh failure rides out on a still-valid sealed access token, but never for force or under 1 minute.
func TestTransientRefreshFallsBackToStaleAccess(t *testing.T) {
	defer func(d time.Duration) { oauth.RetryBackoff = d }(oauth.RetryBackoff)
	oauth.RetryBackoff = time.Millisecond
	ctx := context.Background()
	p, old := staleRow(t, "stale-ok", 5*time.Minute)
	if got, err := userCredential(ctx, p, false); err != nil || got != old {
		t.Errorf("userCredential = %q, %v; want the old sealed token and nil", got, err)
	}
	p, _ = staleRow(t, "stale-force", 5*time.Minute)
	if got, err := userCredential(ctx, p, true); err == nil {
		t.Errorf("force userCredential = %q, nil; want the refresh error", got)
	}
	p, _ = staleRow(t, "stale-short", 30*time.Second)
	if got, err := userCredential(ctx, p, false); err == nil {
		t.Errorf("userCredential with <1m left = %q, nil; want the refresh error", got)
	}
}
