package tailscale

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
)

// staticClient wires a Client at ts.URL with a fixed bearer, bypassing credential resolution.
func staticClient(ts *httptest.Server, bearer string) *Client {
	return &Client{
		BaseURL: ts.URL, HTTP: ts.Client(),
		token: func(context.Context) (string, error) { return bearer, nil },
	}
}

func TestCreateAuthKeyBodyShape(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(AuthKey{ID: "k1", Key: "tskey-auth-xyz"})
	}))
	defer ts.Close()
	c := staticClient(ts, "tok")

	key, err := c.CreateAuthKey(context.Background(), keyOpts{
		Ephemeral: true, Preauthorized: true, Reusable: false,
		Tags: []string{"tag:ci", "tag:ephemeral"}, Expiry: 90 * 24 * time.Hour, Description: "ci runner",
	})
	if err != nil {
		t.Fatalf("CreateAuthKey err = %v", err)
	}
	if key.ID != "k1" || key.Key != "tskey-auth-xyz" {
		t.Errorf("CreateAuthKey result = %+v", key)
	}
	if gotPath != "/tailnet/-/keys" {
		t.Errorf("path = %q, want /tailnet/-/keys", gotPath)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	checkAuthKeyCapabilities(t, gotBody)
	if gotBody["expirySeconds"] != float64(90*24*3600) {
		t.Errorf("expirySeconds = %v, want %d", gotBody["expirySeconds"], 90*24*3600)
	}
	if gotBody["description"] != "ci runner" {
		t.Errorf("description = %v", gotBody["description"])
	}
}

// checkAuthKeyCapabilities asserts the create flags and tags of a CreateAuthKey request body.
func checkAuthKeyCapabilities(t *testing.T, body map[string]any) {
	t.Helper()
	devices, _ := body["capabilities"].(map[string]any)["devices"].(map[string]any)
	create, _ := devices["create"].(map[string]any)
	if create["ephemeral"] != true || create["preauthorized"] != true || create["reusable"] != false {
		t.Errorf("create flags = %+v, want ephemeral+preauthorized, not reusable", create)
	}
	tags, _ := create["tags"].([]any)
	if len(tags) != 2 || tags[0] != "tag:ci" {
		t.Errorf("tags = %v", tags)
	}
}

func TestDevicesParsesList(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tailnet/-/devices" {
			t.Errorf("path = %q", r.URL.Path)
		}
		d := device{ID: "d1", Name: "box.tailnet.ts.net", OS: "linux", Addresses: []string{"100.1.2.3"}}
		if r.URL.Query().Get("fields") == "all" { // like the real API: routes only in the full listing
			d.EnabledRoutes = defaultRoutes
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"devices": []device{d}})
	}))
	defer ts.Close()
	devices, err := staticClient(ts, "tok").Devices(context.Background())
	if err != nil {
		t.Fatalf("Devices err = %v", err)
	}
	if len(devices) != 1 || devices[0].ID != "d1" {
		t.Errorf("Devices = %+v", devices)
	}
	if !hasDefaultRoutes(devices[0].EnabledRoutes) {
		t.Errorf("EnabledRoutes = %v: an approved exit node must read approved", devices[0].EnabledRoutes)
	}
}

func TestApproveExitNodeBody(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.Method == http.MethodGet { // the enabled set is read before it is replaced
			_, _ = w.Write([]byte(`{"advertisedRoutes":["0.0.0.0/0","::/0"],"enabledRoutes":[]}`))
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"advertisedRoutes":["0.0.0.0/0","::/0"],"enabledRoutes":["0.0.0.0/0","::/0"]}`))
	}))
	defer ts.Close()
	if err := staticClient(ts, "tok").ApproveExitNode(context.Background(), "dev123"); err != nil {
		t.Fatalf("ApproveExitNode err = %v", err)
	}
	if gotPath != "/device/dev123/routes" {
		t.Errorf("path = %q, want /device/dev123/routes", gotPath)
	}
	routes, _ := gotBody["routes"].([]any)
	if len(routes) != 2 || routes[0] != "0.0.0.0/0" || routes[1] != "::/0" {
		t.Errorf("routes = %v, want [0.0.0.0/0 ::/0]", routes)
	}
}

// TestRequestRemintsOnceOn401: a 401 triggers exactly one re-mint, then succeeds; a second 401 gives up.
func TestRequestRemintsOnceOn401(t *testing.T) {
	calls := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") == "Bearer fresh" {
			_ = json.NewEncoder(w).Encode(map[string]any{"devices": []device{}})
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer ts.Close()
	mints := 0
	c := &Client{BaseURL: ts.URL, HTTP: ts.Client(), token: func(ctx context.Context) (string, error) {
		mints++
		if force, _ := ctx.Value(forceRefreshKey{}).(bool); force {
			return "fresh", nil
		}
		return "stale", nil
	}}
	if _, err := c.Devices(context.Background()); err != nil {
		t.Fatalf("Devices err = %v", err)
	}
	if calls != 2 {
		t.Errorf("HTTP calls = %d, want 2 (initial 401 + one re-mint retry)", calls)
	}
	if mints != 2 {
		t.Errorf("token mints = %d, want 2 (stale + one forced refresh)", mints)
	}
}

func TestRequestGivesUpAfterSecond401(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer ts.Close()
	c := staticClient(ts, "stale")
	if _, err := c.Devices(context.Background()); err == nil {
		t.Error("Devices() err = nil, want an error after a persistent 401")
	}
}

// TestEnableRoutesKeepsEnabled: approving a subnet must not drop routes the tailnet already enabled.
func TestEnableRoutesKeepsEnabled(t *testing.T) {
	var gotBody map[string]any
	ts := httptest.NewServer(enableRoutesHandler(t, &gotBody))
	defer ts.Close()
	c := staticClient(ts, "tok")
	cur, err := c.Routes(context.Background(), "dev1")
	if err != nil {
		t.Fatalf("Routes err = %v", err)
	}
	enabled, err := c.EnableRoutes(context.Background(), "dev1", cur, []string{"192.168.1.0/24", "::/0"})
	if err != nil {
		t.Fatalf("EnableRoutes err = %v", err)
	}
	routes, _ := gotBody["routes"].([]any)
	if len(routes) != 3 || routes[0] != "0.0.0.0/0" || routes[1] != "::/0" || routes[2] != "192.168.1.0/24" {
		t.Errorf("POST routes = %v, want the enabled pair plus 192.168.1.0/24, no duplicate ::/0", routes)
	}
	if len(enabled) != 3 {
		t.Errorf("enabled after = %v", enabled)
	}
}

// enableRoutesHandler serves GET (two enabled routes) and POST (all three, body captured into gotBody) on dev1.
func enableRoutesHandler(t *testing.T, gotBody *map[string]any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/device/dev1/routes" {
			t.Errorf("path = %q", r.URL.Path)
		}
		switch r.Method {
		case http.MethodGet:
			_, _ = w.Write([]byte(`{"advertisedRoutes":["0.0.0.0/0","::/0","192.168.1.0/24"],` +
				`"enabledRoutes":["0.0.0.0/0","::/0"]}`))
		case http.MethodPost:
			_ = json.NewDecoder(r.Body).Decode(gotBody)
			_, _ = w.Write([]byte(`{"advertisedRoutes":["0.0.0.0/0","::/0","192.168.1.0/24"],` +
				`"enabledRoutes":["0.0.0.0/0","::/0","192.168.1.0/24"]}`))
		}
	}
}

func TestSubnetRoutesDropsDefaults(t *testing.T) {
	got := subnetRoutes([]string{"0.0.0.0/0", "192.168.1.0/24", "::/0", "10.9.0.0/16"})
	if len(got) != 2 || got[0] != "192.168.1.0/24" || got[1] != "10.9.0.0/16" {
		t.Errorf("SubnetRoutes = %v", got)
	}
}

// hermeticSecrets blocks the keyring and file plane so vault.Get resolves from the process env only.
func hermeticSecrets(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	for _, n := range []string{"TAILSCALE_OAUTH_CLIENT_ID", "TAILSCALE_OAUTH_CLIENT_SECRET", "TAILSCALE_API_KEY"} {
		t.Setenv(n, "")
	}
}

func TestNewClientErrNoCredential(t *testing.T) {
	hermeticSecrets(t)
	if _, err := NewClient(); !errors.Is(err, errNoCredential) {
		t.Errorf("NewClient() err = %v, want ErrNoCredential", err)
	}
}

// NewClient's OAuth branch is untested here: its token func would POST to the real defaultBaseURL.

func TestNewClientFallsBackToAPIKey(t *testing.T) {
	hermeticSecrets(t)
	t.Setenv("TAILSCALE_API_KEY", "tskey-api-abc")
	c, err := NewClient()
	if err != nil {
		t.Fatal(err)
	}
	tok, err := c.token(context.Background())
	if err != nil || tok != "tskey-api-abc" {
		t.Errorf("token = (%q, %v), want (tskey-api-abc, nil)", tok, err)
	}
}

func TestParseKeyExpiry(t *testing.T) {
	for in, want := range map[string]time.Duration{"90d": 90 * 24 * time.Hour, "36h": 36 * time.Hour} {
		if got, err := parseKeyExpiry(in); err != nil || got != want {
			t.Errorf("ParseKeyExpiry(%q) = (%v, %v), want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"xd", "soon", "", "-5d", "-1h", "0", "0d", "0s", "500ms", "999ms"} {
		if _, err := parseKeyExpiry(bad); err == nil {
			t.Errorf("ParseKeyExpiry(%q) accepted", bad)
		}
	}
}

func TestSelfDeviceNoCredential(t *testing.T) {
	hermeticSecrets(t)
	if _, _, _, err := selfDevice(context.Background()); !errors.Is(err, errNoCredential) {
		t.Errorf("selfDevice err = %v, want ErrNoCredential", err)
	}
}

func TestSetNames(t *testing.T) {
	if got := SetNames(true); len(got) != 1 || got[0] != "TAILSCALE_API_KEY" {
		t.Errorf("SetNames(true) = %v", got)
	}
	if got := SetNames(false); len(got) != 2 || got[0] != "TAILSCALE_OAUTH_CLIENT_ID" ||
		got[1] != "TAILSCALE_OAUTH_CLIENT_SECRET" {
		t.Errorf("SetNames(false) = %v", got)
	}
}

// EnableRoutes must not write into the caller's cur.Enabled backing array.
func TestEnableRoutesDoesNotMutateCaller(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"enabledRoutes":[]}`))
	}))
	defer ts.Close()
	backing := make([]string, 1, 4)
	backing[0] = "::/0"
	cur := deviceRoutes{Enabled: backing}
	if _, err := staticClient(ts, "tok").EnableRoutes(context.Background(), "d", cur, []string{"10.0.0.0/8"}); err != nil {
		t.Fatal(err)
	}
	if got := backing[:2][1]; got != "" {
		t.Errorf("caller's backing array overwritten with %q", got)
	}
}

func TestRequestErrorBodyIsCapped(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write(make([]byte, 1<<20))
	}))
	defer ts.Close()
	err := staticClient(ts, "tok").request(context.Background(), http.MethodGet, "/x", nil, nil)
	if err == nil || len(err.Error()) > 8<<10 {
		t.Fatalf("want error with body capped near 4KiB, got %v", err != nil)
	}
}

func TestMain(m *testing.M) { os.Exit(hosttest.Main(m)) }

// TestOAuthTokenMintsAndCachesUntilNearExpiry: one form-encoded mint per 2s token, reused within it.
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
	if gotForm.Get("client_id") != "client-id" || gotForm.Get("client_secret") != "client-secret" ||
		gotForm.Get("grant_type") != "client_credentials" {
		t.Errorf("form = %v, want client_id/client_secret and grant_type=client_credentials", gotForm)
	}
	// Second call within the cache window must not hit the server again.
	if tok2, err := fn(context.Background()); err != nil || tok2 != "minted-1" || mints != 1 {
		t.Errorf("fn() (cached) = (%q, %v), mints=%d, want cached minted-1 with 1 mint", tok2, err, mints)
	}
}

// TestOAuthTokenForceRefreshBypassesCache: withForceRefresh re-mints a fresh token (the post-401 path).
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

// A token response over the 64 KiB cap is truncated and fails to decode instead of being read whole.
func TestMintTokenCapsResponseBody(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"` + strings.Repeat("a", 200<<10) + `"}`))
	}))
	defer ts.Close()
	if _, err := mintToken(context.Background(), ts.URL, "id", "secret", ts.Client()); err == nil {
		t.Fatal("mintToken accepted a 200 KiB body, want a decode error")
	}
}
