package tailscale

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubOAuthMint points apiBaseURL at an httptest server minting an access_token, restored on cleanup.
func stubOAuthMint(t *testing.T) {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	t.Cleanup(ts.Close)
	old := apiBaseURL
	apiBaseURL = ts.URL
	t.Cleanup(func() { apiBaseURL = old })
}

func TestDoctorNoCLI(t *testing.T) {
	noCLI(t)
	hermeticSecrets(t)
	ok, detail := Doctor(context.Background())
	if ok {
		t.Error("Doctor() ok = true, want false (no CLI)")
	}
	if !strings.Contains(detail, "cli not found") || !strings.Contains(detail, "no admin credential") {
		t.Errorf("detail = %q", detail)
	}
}

func TestDoctorRunningAndApproved(t *testing.T) {
	stubCLI(t, catScript(t, "status_running.json"))
	stubOAuthMint(t)
	hermeticSecrets(t)
	t.Setenv("TAILSCALE_OAUTH_CLIENT_ID", "kJaxHLLXzN11CNTRL")
	t.Setenv("TAILSCALE_OAUTH_CLIENT_SECRET", "tskey-client-set")
	ok, detail := Doctor(context.Background())
	if !ok {
		t.Errorf("Doctor() ok = false, detail = %q", detail)
	}
	for _, want := range []string{"Running", "exit-node approved",
		"api credential TAILSCALE_OAUTH_CLIENT_ID+SECRET verified"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail %q missing %q", detail, want)
		}
	}
}

// A half-set OAuth pair plus an API key falls back to the API-key note, as NewClient does.
func TestDoctorHalfOAuthPlusAPIKeyFallsBackToAPIKeyNote(t *testing.T) {
	stubCLI(t, catScript(t, "status_running.json"))
	hermeticSecrets(t)
	t.Setenv("TAILSCALE_OAUTH_CLIENT_ID", "kJaxHLLXzN11CNTRL")
	t.Setenv("TAILSCALE_API_KEY", "tskey-api-set")
	_, detail := Doctor(context.Background())
	if !strings.Contains(detail, "api credential TAILSCALE_API_KEY") {
		t.Errorf("detail = %q, want it to fall back to the API-key note", detail)
	}
	if strings.Contains(detail, "incomplete") {
		t.Errorf("detail = %q, must not report incomplete once an API key covers it", detail)
	}
}

// A half-set OAuth pair (ID or secret alone, say a truncated `vault set` name) with no API key is incomplete.
func TestDoctorHalfOAuthWithoutAPIKeyStillIncomplete(t *testing.T) {
	for _, env := range []string{"TAILSCALE_OAUTH_CLIENT_ID", "TAILSCALE_OAUTH_CLIENT_SECRET"} {
		t.Run(env, func(t *testing.T) {
			stubCLI(t, catScript(t, "status_running.json"))
			hermeticSecrets(t)
			t.Setenv(env, "set")
			_, detail := Doctor(context.Background())
			if !strings.Contains(detail, "api credential incomplete") {
				t.Errorf("detail = %q, want incomplete", detail)
			}
		})
	}
}

// A well-formed OAuth pair that the API itself refuses (401) is reported "rejected", not present.
func TestDoctorOAuthPairRejectedByAPI(t *testing.T) {
	stubCLI(t, catScript(t, "status_running.json"))
	hermeticSecrets(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer ts.Close()
	old := apiBaseURL
	apiBaseURL = ts.URL
	t.Cleanup(func() { apiBaseURL = old })
	t.Setenv("TAILSCALE_OAUTH_CLIENT_ID", "kJaxHLLXzN11CNTRL")
	t.Setenv("TAILSCALE_OAUTH_CLIENT_SECRET", "tskey-client-set")
	ok, detail := Doctor(context.Background())
	if ok {
		t.Errorf("Doctor() ok = true, want false (API rejected the credential)")
	}
	if !strings.Contains(detail, "rejected") {
		t.Errorf("detail = %q, want it to report the credential as rejected", detail)
	}
}

// A malformed OAuth secret never reaches the network — the shape check short-circuits before any mint.
func TestDoctorMalformedCredentialNeverCallsAPI(t *testing.T) {
	stubCLI(t, catScript(t, "status_running.json"))
	hermeticSecrets(t)
	var hits int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	defer ts.Close()
	old := apiBaseURL
	apiBaseURL = ts.URL
	t.Cleanup(func() { apiBaseURL = old })
	t.Setenv("TAILSCALE_OAUTH_CLIENT_ID", "kJaxHLLXzN11CNTRL")
	t.Setenv("TAILSCALE_OAUTH_CLIENT_SECRET", "not-shaped-like-a-secret")
	ok, detail := Doctor(context.Background())
	if ok {
		t.Errorf("Doctor() ok = true, want false (malformed secret)")
	}
	if !strings.Contains(detail, "malformed") {
		t.Errorf("detail = %q, want it to report the credential as malformed", detail)
	}
	if hits != 0 {
		t.Errorf("hits = %d, want 0: a malformed credential must never reach the network", hits)
	}
}

// Neither an API key alone nor no credential at all calls the network — only the OAuth pair mints a token.
func TestDoctorWithoutOAuthPairNeverCallsNetwork(t *testing.T) {
	const apiKey = "tskey-api-should-never-appear"
	for name, key := range map[string]string{"api key only": apiKey, "no credential": ""} {
		t.Run(name, func(t *testing.T) {
			stubCLI(t, catScript(t, "status_running.json"))
			hermeticSecrets(t)
			var hits int
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
			defer ts.Close()
			old := apiBaseURL
			apiBaseURL = ts.URL
			t.Cleanup(func() { apiBaseURL = old })
			if key != "" {
				t.Setenv("TAILSCALE_API_KEY", key)
			}
			ok, detail := Doctor(context.Background())
			if !ok {
				t.Errorf("Doctor() ok = false, detail = %q", detail)
			}
			if hits != 0 {
				t.Errorf("hits = %d, want 0: without an OAuth pair nothing may mint a token", hits)
			}
			if key != "" && strings.Contains(detail, key) {
				t.Errorf("detail leaked the credential value: %q", detail)
			}
		})
	}
}
