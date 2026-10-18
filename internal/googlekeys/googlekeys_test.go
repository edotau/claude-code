package googlekeys

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testAccessToken = "ya29.secret-access-token"

func writeADC(t *testing.T, body string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "adc.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", p)
}

const userADC = `{"type":"authorized_user","client_id":"cid","client_secret":"csec","refresh_token":"rtok","quota_project_id":"quota-proj"}`

// fakeGoogle serves the token endpoint and handler h for the API Keys API, and points the package vars at it.
func fakeGoogle(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "rtok" ||
			r.Form.Get("client_id") != "cid" || r.Form.Get("client_secret") != "csec" {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error_description":"bad refresh form"}`))
			return
		}
		w.Write([]byte(`{"access_token":"` + testAccessToken + `"}`))
	})
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testAccessToken {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		h(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	oldTok, oldAPI := TokenURL, APIKeysURL
	TokenURL, APIKeysURL = srv.URL+"/token", srv.URL+"/v2"
	t.Cleanup(func() { TokenURL, APIKeysURL = oldTok, oldAPI })
}

func TestNewErrors(t *testing.T) {
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")
	cases := []struct {
		name, adc, want string
	}{
		{"service account refused", `{"type":"service_account"}`, "authorized_user"},
		{"no project", `{"type":"authorized_user","refresh_token":"r"}`, "no Google Cloud project"},
		{"bad json", `{`, "parse"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			writeADC(t, c.adc)
			_, err := New(context.Background())
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want containing %q", err, c.want)
			}
		})
	}
}

func TestNewNoADC(t *testing.T) {
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", filepath.Join(t.TempDir(), "missing.json"))
	if _, err := New(context.Background()); err != ErrNoADC {
		t.Fatalf("err = %v, want ErrNoADC", err)
	}
}

func TestADCPathPrecedence(t *testing.T) {
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	t.Setenv("CLOUDSDK_CONFIG", "/cfg")
	if got := ADCPath(); got != "/cfg/application_default_credentials.json" {
		t.Errorf("ADCPath = %q", got)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/explicit.json")
	if got := ADCPath(); got != "/explicit.json" {
		t.Errorf("ADCPath = %q", got)
	}
}

func TestNewProjectPrecedence(t *testing.T) {
	fakeGoogle(t, func(http.ResponseWriter, *http.Request) {})
	writeADC(t, userADC)
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")
	c, err := New(context.Background())
	if err != nil || c.Project != "quota-proj" {
		t.Fatalf("project = %v, err %v, want quota-proj", c, err)
	}
	t.Setenv("GOOGLE_CLOUD_PROJECT", " env-proj ")
	c, err = New(context.Background())
	if err != nil || c.Project != "env-proj" {
		t.Fatalf("project = %v, err %v, want env-proj", c, err)
	}
}

func TestNewRefreshFailureHidesSecrets(t *testing.T) {
	fakeGoogle(t, func(http.ResponseWriter, *http.Request) {})
	writeADC(t, strings.Replace(userADC, `"rtok"`, `"wrong"`, 1))
	_, err := New(context.Background())
	if err == nil || !strings.Contains(err.Error(), "bad refresh form") {
		t.Fatalf("err = %v, want Google's description", err)
	}
	if strings.Contains(err.Error(), "wrong") || strings.Contains(err.Error(), "csec") {
		t.Errorf("error leaks credentials: %v", err)
	}
}

func newClient(t *testing.T) *Client {
	t.Helper()
	writeADC(t, userADC)
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")
	c, err := New(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCreatePollsAndReadsKeyString(t *testing.T) {
	polls := 0
	fakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-goog-user-project") != "quota-proj" {
			t.Errorf("x-goog-user-project = %q", r.Header.Get("x-goog-user-project"))
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v2/projects/quota-proj/locations/global/keys":
			var body struct {
				DisplayName  string `json:"displayName"`
				Restrictions struct {
					APITargets []struct {
						Service string `json:"service"`
					} `json:"apiTargets"`
				} `json:"restrictions"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode body: %v", err)
			}
			if body.DisplayName != "my key" || len(body.Restrictions.APITargets) != 1 ||
				body.Restrictions.APITargets[0].Service != GeminiService {
				t.Errorf("body = %+v, want one apiTarget %s", body, GeminiService)
			}
			w.Write([]byte(`{"name":"operations/op1","done":false}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v2/operations/op1":
			polls++
			w.Write([]byte(`{"name":"operations/op1","done":true,"response":{"name":"projects/1/locations/global/keys/k1"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v2/projects/1/locations/global/keys/k1/keyString":
			w.Write([]byte(`{"keyString":"AIza-new"}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	c := newClient(t)
	name, key, err := c.Create(context.Background(), "my key")
	if err != nil {
		t.Fatal(err)
	}
	if name != "projects/1/locations/global/keys/k1" || key != "AIza-new" || polls != 1 {
		t.Errorf("name=%q key=%q polls=%d", name, key, polls)
	}
}

func TestLookupAndDelete(t *testing.T) {
	fakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/keys:lookupKey":
			if r.URL.Query().Get("keyString") != "AIza old+key" {
				t.Errorf("keyString = %q", r.URL.Query().Get("keyString"))
			}
			w.Write([]byte(`{"name":"projects/1/locations/global/keys/old"}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/v2/projects/1/locations/global/keys/old":
			w.Write([]byte(`{"name":"operations/del","done":true,"response":{}}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
		}
	})
	c := newClient(t)
	name, err := c.Lookup(context.Background(), "AIza old+key")
	if err != nil || name != "projects/1/locations/global/keys/old" {
		t.Fatalf("Lookup = %q, %v", name, err)
	}
	if err := c.Delete(context.Background(), name); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

func TestHTTPErrorSurfacesMessageNotToken(t *testing.T) {
	fakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":{"message":"API Keys API has not been used in project 1"}}`))
	})
	c := newClient(t)
	_, _, err := c.Create(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "HTTP 403: API Keys API has not been used") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), testAccessToken) {
		t.Errorf("error leaks the access token: %v", err)
	}
	if err := c.Delete(context.Background(), "projects/1/locations/global/keys/x"); err == nil {
		t.Error("Delete: want error")
	}
}

func TestOperationErrorSurfaces(t *testing.T) {
	fakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"name":"operations/x","done":true,"error":{"message":"quota exceeded"}}`))
	})
	_, _, err := newClient(t).Create(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "quota exceeded") {
		t.Fatalf("err = %v", err)
	}
}
