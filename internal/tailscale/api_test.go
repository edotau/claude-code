package tailscale

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// staticClient wires a Client at ts.URL with a fixed bearer, bypassing credential resolution.
func staticClient(ts *httptest.Server, bearer string) *Client {
	return &Client{
		BaseURL: ts.URL, Tailnet: "-", HTTP: ts.Client(),
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

	key, err := c.CreateAuthKey(context.Background(), KeyOpts{
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
	devices, _ := gotBody["capabilities"].(map[string]any)["devices"].(map[string]any)
	create, _ := devices["create"].(map[string]any)
	if create["ephemeral"] != true || create["preauthorized"] != true || create["reusable"] != false {
		t.Errorf("create flags = %+v, want ephemeral+preauthorized, not reusable", create)
	}
	tags, _ := create["tags"].([]any)
	if len(tags) != 2 || tags[0] != "tag:ci" {
		t.Errorf("tags = %v", tags)
	}
	if gotBody["expirySeconds"] != float64(90*24*3600) {
		t.Errorf("expirySeconds = %v, want %d", gotBody["expirySeconds"], 90*24*3600)
	}
	if gotBody["description"] != "ci runner" {
		t.Errorf("description = %v", gotBody["description"])
	}
}

func TestDevicesParsesList(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tailnet/-/devices" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"devices": []Device{{ID: "d1", Name: "box.tailnet.ts.net", OS: "linux", Addresses: []string{"100.1.2.3"}}},
		})
	}))
	defer ts.Close()
	devices, err := staticClient(ts, "tok").Devices(context.Background())
	if err != nil {
		t.Fatalf("Devices err = %v", err)
	}
	if len(devices) != 1 || devices[0].ID != "d1" {
		t.Errorf("Devices = %+v", devices)
	}
}

func TestApproveExitNodeBody(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
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
			_ = json.NewEncoder(w).Encode(map[string]any{"devices": []Device{}})
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer ts.Close()
	mints := 0
	c := &Client{BaseURL: ts.URL, Tailnet: "-", HTTP: ts.Client(), token: func(ctx context.Context) (string, error) {
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
