package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/gateway"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// clientTokenOverlay is a Databricks serving row dev and a plain anthropic row sub, both on up.
func clientTokenOverlay(up string) string {
	serving := up + "/serving-endpoints"
	return `{"default":"dev","providers":{"databricks":{"dev":{"preset":"databricks",` +
		`"host":"https://example.cloud.databricks.com","base_url":"` + serving + `/anthropic",` +
		`"routes":{"openai":"` + serving + `","responses":"` + serving + `"},` +
		`"auth":{"type":"key","env":"DEV_TOKEN"},"models":{"opus":"databricks-claude-opus-5-5"}}},` +
		`"anthropic":{"sub":{"protocol":"anthropic","base_url":"` + up + `","auth":{"type":"key","env":"SUB_KEY"},` +
		`"models":{"opus":"m1"}}}}}`
}

// A client token is refused (401 wrong, 403 out of scope) before any upstream call and never mints a session.
func TestClientTokenRefusals(t *testing.T) {
	useGate(t)
	up := newRawUp(t, okRaw)
	t.Setenv("DEV_TOKEN", "dev-workspace-token")
	t.Setenv("SUB_KEY", "sub-key")
	srv := setupRouter(t, clientTokenOverlay(up.URL), nil)
	tok, err := MintClientToken("vscode")
	if err != nil {
		t.Fatal(err)
	}
	const msg = `{"model":"databricks-claude-opus-5-5","max_tokens":5}`
	for _, c := range []struct {
		path, tok string
		want      int
	}{
		{"/p/dev/v1/messages", clientTokenPf + strings.Repeat("0", 64), http.StatusUnauthorized},
		{"/p/dev/v1/messages", strings.TrimPrefix(tok, clientTokenPf), http.StatusUnauthorized},
		{"/p/sub/v1/messages", tok, http.StatusForbidden},
		{"/p/nosuch/v1/messages", tok, http.StatusForbidden},
		{"/v1/messages", tok, http.StatusForbidden},
		{gateway.SessionPath, tok, http.StatusUnauthorized},
	} {
		resp := ptDo(t, http.MethodPost, srv.URL+c.path, msg, map[string]string{"X-Api-Key": c.tok})
		if got := body(t, resp); resp.StatusCode != c.want || strings.Contains(got, tok) {
			t.Errorf("%s = %d %s, want %d without the token", c.path, resp.StatusCode, got, c.want)
		}
	}
	if n := len(up.got()); n != 0 {
		t.Fatalf("refused client tokens reached the upstream %d times, want 0", n)
	}
}

// A minted client token reaches a Databricks serving row (Messages, chat) with its workspace credential until revoked.
func TestClientTokenAcceptedOnDatabricksUntilRevoked(t *testing.T) {
	useGate(t)
	up := newRawUp(t, okRaw)
	t.Setenv("DEV_TOKEN", "dev-workspace-token")
	t.Setenv("SUB_KEY", "sub-key")
	vaulttest.UseKeyring(t)
	tok, err := MintClientToken("VS-Code") // minted before the router starts: read from the vault, as after a restart
	if err != nil {
		t.Fatal(err)
	}
	srv := setupRouter2(t, clientTokenOverlay(up.URL))
	const msg = `{"model":"databricks-claude-opus-5-5","max_tokens":5}`
	cases := []struct{ path, hdr, val, want string }{
		{"/p/dev/v1/messages", "X-Api-Key", tok, "/serving-endpoints/anthropic/v1/messages"},
		{"/p/dev/openai/chat/completions", "Authorization", "Bearer " + tok, "/serving-endpoints/chat/completions"},
	}
	for i, c := range cases {
		resp := ptDo(t, http.MethodPost, srv.URL+c.path, msg, map[string]string{"X-Api-Key": "", c.hdr: c.val})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s = %d %s, want %d", c.path, resp.StatusCode, body(t, resp), http.StatusOK)
		}
		got := up.got()
		if len(got) != i+1 || got[i].uri != c.want || got[i].hdr.Get("Authorization") != "Bearer dev-workspace-token" {
			t.Fatalf("%s upstream = %+v, want %s with dev's workspace credential", c.path, got[len(got)-1], c.want)
		}
		assertNoToken(t, got[i].hdr, tok)
	}
	if existed, err := RevokeClientToken("vs-code"); err != nil || !existed {
		t.Fatalf("revoke = %v, %v; want true, nil", existed, err)
	}
	resp := ptDo(t, http.MethodPost, srv.URL+"/p/dev/v1/messages", msg, map[string]string{"X-Api-Key": tok})
	if got := body(t, resp); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked token = %d %s, want %d", resp.StatusCode, got, http.StatusUnauthorized)
	}
}

// The vault holds only the token's SHA-256 under the normalized name; a bad name mints nothing.
func TestMintClientTokenStoresOnlyTheHash(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	vaulttest.UseKeyring(t)
	for _, bad := range []string{"", "a/b", "-x", "a_b", strings.Repeat("a", 33)} {
		if _, err := MintClientToken(bad); err == nil {
			t.Errorf("MintClientToken(%q) succeeded, want a name error", bad)
		}
	}
	tok, err := MintClientToken("vscode")
	if err != nil {
		t.Fatal(err)
	}
	stored, found, err := vault.ReadSecret("HARNESS_PROXY_CLIENT_VSCODE")
	if err != nil || !found || stored != textutil.SHA256Hex([]byte(tok)) {
		t.Fatalf("stored = %q found %v err %v, want the token's sha256", stored, found, err)
	}
}

// assertNoToken fails when any upstream header value carries tok.
func assertNoToken(t *testing.T, hdr http.Header, tok string) {
	t.Helper()
	for k, v := range hdr {
		if strings.Contains(strings.Join(v, ","), tok) {
			t.Errorf("upstream header %s carries the client token", k)
		}
	}
}
