package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestProbeEndpointsClientRouteParity(t *testing.T) {
	for _, tc := range []struct {
		name, id, path, body string
		responses            bool
		anthropic            bool
	}{
		{"chat", "gpt-5-1", "/serving-endpoints/custom/chat/chat/completions",
			`{"messages":[{"role":"user","content":"Reply with OK."}],"max_tokens":256,"tools":[{"type":"function","function":{"name":"ping","description":"Return pong.","parameters":{"type":"object","properties":{}}}}],"tool_choice":"auto"}`, false, false},
		{"native-claude", "claude-sonnet-5-5", "/serving-endpoints/custom/v1/messages",
			`{"messages":[{"role":"user","content":"Reply with OK."}],"max_tokens":32,"tools":[{"name":"ping","description":"Return pong.","input_schema":{"type":"object","properties":{}}}],"tool_choice":{"type":"auto"}}`, false, true},
		{"responses", "gpt-5-5-pro", "/serving-endpoints/custom-responses/responses",
			`{"input":"Reply with OK.","max_output_tokens":128,"tools":[{"type":"function","name":"ping","description":"Return pong.","parameters":{"type":"object","properties":{}}}]}`, true, false},
		{"explicit-responses-before-claude", "claude-sonnet-5-5", "/serving-endpoints/custom-responses/responses",
			`{"input":"Reply with OK.","max_output_tokens":128,"tools":[{"type":"function","name":"ping","description":"Return pong.","parameters":{"type":"object","properties":{}}}]}`, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vaulttest.Use(t)
			t.Setenv("CLIENT_ROUTE_TOKEN", "synthetic-route-token")
			type request struct {
				method, path string
				header       http.Header
				body         map[string]any
			}
			requests := make(chan request, 4)
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("probe body: %v", err)
				}
				requests <- request{r.Method, r.URL.RequestURI(), r.Header.Clone(), body}
				_, _ = w.Write([]byte(`{"ok":true}`))
			}))
			t.Cleanup(srv.Close)
			// Exercise both the primary BaseURL and relative/absolute p.Route overrides.
			p := &providers.Provider{Name: "client-routes", Preset: providers.PresetDatabricks, Host: srv.URL,
				Protocol: providers.ProtocolAnthropic, BaseURL: srv.URL + "/serving-endpoints/custom",
				Routes: map[string]string{providers.ProtocolOpenAI: "chat", providers.ProtocolResponses: srv.URL + "/serving-endpoints/custom-responses"},
				Auth:   providers.Auth{Type: providers.AuthKey, Env: "CLIENT_ROUTE_TOKEN"}}
			target := CheckTarget{ID: tc.id, Wire: "databricks-" + tc.id, Responses: tc.responses}
			got, err := ProbeEndpoints(context.Background(), p, []CheckTarget{target}, CheckOptions{Client: srv.Client(), ClientRoutes: true})
			if err != nil || len(got) != 1 || got[0].Status != "ok" || got[0].HTTP != 200 || got[0].Tools != "yes" {
				t.Fatalf("ProbeEndpoints = %+v, %v", got, err)
			}
			if len(requests) != 1 {
				t.Fatalf("requests = %d, want one", len(requests))
			}
			r := <-requests
			if r.method != "POST" || r.path != tc.path || r.header.Get("Content-Type") != "application/json" || r.header.Get("Authorization") != "Bearer synthetic-route-token" {
				t.Errorf("wrong method/route/headers: %s %s", r.method, r.path)
			}
			version := ""
			if tc.anthropic {
				version = "2023-06-01"
			}
			if r.header.Get("anthropic-version") != version {
				t.Errorf("anthropic-version = %q, want %q", r.header.Get("anthropic-version"), version)
			}
			var want map[string]any
			if err := json.Unmarshal([]byte(tc.body), &want); err != nil {
				t.Fatal(err)
			}
			want["model"] = target.Wire
			if !reflect.DeepEqual(r.body, want) {
				t.Errorf("client body = %#v, want %#v", r.body, want)
			}
		})
	}
}

func TestProbeEndpointsClientRoutesRejectUnsafeDestinations(t *testing.T) {
	vaulttest.Use(t)
	t.Setenv("CLIENT_ROUTE_TOKEN", "synthetic-route-token")
	var calls, redirected atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/redirected" {
			redirected.Add(1)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		http.Redirect(w, r, "/redirected", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(srv.Close)
	p := providers.Provider{Name: "safe-routes", Preset: providers.PresetDatabricks, Host: srv.URL,
		Protocol: providers.ProtocolOpenAI, BaseURL: srv.URL + "/serving-endpoints",
		Auth: providers.Auth{Type: providers.AuthKey, Env: "CLIENT_ROUTE_TOKEN"}}
	targets := []CheckTarget{{ID: "glm-5-2", Wire: "databricks-glm-5-2"}}
	for _, tc := range []struct {
		name string
		edit func(*providers.Provider)
	}{
		{"gateway", func(p *providers.Provider) { p.Options.Wire = providers.WireAIGateway }},
		{"off-workspace", func(p *providers.Provider) { p.BaseURL = "https://127.0.0.1:1/serving-endpoints" }},
		{"outside-serving", func(p *providers.Provider) { p.BaseURL = srv.URL + "/ai-gateway" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			other := p
			tc.edit(&other)
			// Reject every transport call: these guards must run before sending credentials.
			client := &http.Client{Transport: discoveryTransport(func(*http.Request) (*http.Response, error) {
				t.Error("invalid route reached the HTTP transport")
				return nil, context.Canceled
			})}
			if _, err := ProbeEndpoints(context.Background(), &other, targets, CheckOptions{Client: client, ClientRoutes: true}); err == nil {
				t.Fatal("unsafe route accepted")
			}
		})
	}
	got, err := ProbeEndpoints(context.Background(), &p, targets, CheckOptions{Client: srv.Client(), ClientRoutes: true})
	if err != nil || len(got) != 1 || got[0].Status != "fail" || got[0].HTTP != 307 || calls.Load() != 2 || redirected.Load() != 0 {
		t.Fatalf("redirect must fail both tool/no-tool attempts without following: results=%+v err=%v calls=%d redirected=%d", got, err, calls.Load(), redirected.Load())
	}
}
