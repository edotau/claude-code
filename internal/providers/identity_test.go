package providers

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/databricks/oauth"
)

// spHarness seals a refresh token and an SP secret on a paired u2m row; refresh is rejected (401), M2M is counted.
func spHarness(t *testing.T, identity string) (p *Provider, m2mCalls, refreshCalls *int, note *bytes.Buffer) {
	t.Helper()
	srv := oidcServer(t, func(*http.Request, url.Values) (int, map[string]any) { return 401, map[string]any{} })
	p = mintRow(t, "sp-"+strings.ReplaceAll(t.Name(), "/", "-"), AuthOAuth, "", srv.URL)
	p.Identity, p.Auth.SPClientID = identity, "sp-client"
	if err := writeOAuth(p, oauthValue{Refresh: "user-refresh"}); err != nil {
		t.Fatal(err)
	}
	if err := SealSecret(SecretName(GroupDatabricks, p.Name, AuthM2M), "sp-secret"); err != nil {
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
