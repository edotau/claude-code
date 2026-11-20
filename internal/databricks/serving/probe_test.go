package serving

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

type clientRouteCase struct {
	name, id, path, body string
	responses            bool
	anthropic            bool
}

type seenProbe struct {
	method, path string
	header       http.Header
	body         map[string]any
}

var clientRouteCases = []clientRouteCase{
	{"chat", "gpt-5-1", "/serving-endpoints/custom/chat/chat/completions",
		`{"messages":[{"role":"user","content":"Reply with OK."}],"max_tokens":256,"tools":[{"type":"function","function":{"name":"ping","description":"Return pong.","parameters":{"type":"object","properties":{}}}}],"tool_choice":"auto"}`, false, false},
	{"native-claude", "claude-sonnet-5-5", "/serving-endpoints/custom/v1/messages",
		`{"messages":[{"role":"user","content":"Reply with OK."}],"max_tokens":32,"tools":[{"name":"ping","description":"Return pong.","input_schema":{"type":"object","properties":{}}}],"tool_choice":{"type":"auto"}}`, false, true},
	{"responses", "gpt-5-5-pro", "/serving-endpoints/custom-responses/responses",
		`{"input":"Reply with OK.","max_output_tokens":128,"tools":[{"type":"function","name":"ping","description":"Return pong.","parameters":{"type":"object","properties":{}}}]}`, true, false},
	{"explicit-responses-before-claude", "claude-sonnet-5-5", "/serving-endpoints/custom-responses/responses",
		`{"input":"Reply with OK.","max_output_tokens":128,"tools":[{"type":"function","name":"ping","description":"Return pong.","parameters":{"type":"object","properties":{}}}]}`, true, false},
}

// probeEnv is a sealed test vault, a client-route token and a TLS workspace served by h, plus a serving provider.
func probeEnv(t *testing.T, name string, h http.HandlerFunc) (*providers.Provider, *httptest.Server) {
	t.Helper()
	vaulttest.Use(t)
	t.Setenv("CLIENT_ROUTE_TOKEN", "synthetic-route-token")
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	return &providers.Provider{Name: name, Preset: providers.PresetDatabricks, Host: srv.URL,
		Protocol: providers.ProtocolOpenAI, BaseURL: srv.URL + "/serving-endpoints",
		Auth: providers.Auth{Type: providers.AuthKey, Env: "CLIENT_ROUTE_TOKEN"}}, srv
}

func TestProbeClientRouteParity(t *testing.T) {
	for _, tc := range clientRouteCases {
		t.Run(tc.name, func(t *testing.T) {
			requests := make(chan seenProbe, 4)
			p, srv := probeEnv(t, "client-routes", func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("probe body: %v", err)
				}
				requests <- seenProbe{r.Method, r.URL.RequestURI(), r.Header.Clone(), body}
				writeSSE(w, tc.responses, tc.anthropic, true)
			})
			// Exercise both the primary BaseURL and relative/absolute p.Route overrides.
			p.Protocol, p.BaseURL = providers.ProtocolAnthropic, srv.URL+"/serving-endpoints/custom"
			p.Routes = map[string]string{providers.ProtocolOpenAI: "chat", providers.ProtocolResponses: srv.URL + "/serving-endpoints/custom-responses"}
			target := CheckTarget{ID: tc.id, Wire: "databricks-" + tc.id, Responses: tc.responses}
			got, err := Probe(context.Background(), p, []CheckTarget{target}, CheckOptions{Client: srv.Client(), ClientRoutes: true})
			if err != nil || len(got) != 1 || got[0].Status != "ok" || got[0].HTTP != 200 || got[0].Tools != "yes" {
				t.Fatalf("Probe = %+v, %v", got, err)
			}
			if len(requests) != 1 {
				t.Fatalf("requests = %d, want one", len(requests))
			}
			checkProbeRequest(t, <-requests, tc, target.Wire)
		})
	}
}

// checkProbeRequest checks the probe's method, route, headers and body against the case's expectations.
func checkProbeRequest(t *testing.T, r seenProbe, tc clientRouteCase, wire string) {
	t.Helper()
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
	want["model"], want["stream"] = wire, true
	if !reflect.DeepEqual(r.body, want) {
		t.Errorf("client body = %#v, want %#v", r.body, want)
	}
}

func TestProbeClientRoutesRejectUnsafeDestinations(t *testing.T) {
	var calls, redirected atomic.Int32
	pp, srv := probeEnv(t, "safe-routes", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/redirected" {
			redirected.Add(1)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		http.Redirect(w, r, "/redirected", http.StatusTemporaryRedirect)
	})
	p := *pp
	targets := []CheckTarget{{ID: "glm-5-2", Wire: "databricks-glm-5-2"}}
	for _, tc := range []struct {
		name string
		edit func(*providers.Provider)
	}{
		{"off-workspace", func(p *providers.Provider) { p.BaseURL = "https://127.0.0.1:1/serving-endpoints" }},
		{"outside-serving", func(p *providers.Provider) { p.BaseURL = srv.URL + "/other" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			other := p
			tc.edit(&other)
			// Reject every transport call: these guards must run before sending credentials.
			client := &http.Client{Transport: discoveryTransport(func(*http.Request) (*http.Response, error) {
				t.Error("invalid route reached the HTTP transport")
				return nil, context.Canceled
			})}
			if _, err := Probe(context.Background(), &other, targets, CheckOptions{Client: client, ClientRoutes: true}); err == nil {
				t.Fatal("unsafe route accepted")
			}
		})
	}
	got, err := Probe(context.Background(), &p, targets, CheckOptions{Client: srv.Client(), ClientRoutes: true})
	if err != nil || len(got) != 1 || got[0].Status != "fail" || got[0].HTTP != 307 || calls.Load() != 2 || redirected.Load() != 0 {
		t.Fatalf("redirect must fail both tool/no-tool attempts without following: results=%+v err=%v calls=%d redirected=%d", got, err, calls.Load(), redirected.Load())
	}
}

func TestProbeTransientWithToolsSkipsNoToolsRetry(t *testing.T) {
	var calls atomic.Int32
	p, srv := probeEnv(t, "transient", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if _, withTools := body["tools"]; withTools {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	targets := []CheckTarget{{ID: "glm-5-2", Wire: "databricks-glm-5-2"}}
	got, err := Probe(context.Background(), p, targets, CheckOptions{Client: srv.Client(), ClientRoutes: true})
	if err != nil || len(got) != 1 || got[0].Status != StatusTransient || got[0].Tools != "" || got[0].HTTP != 429 || calls.Load() != 1 {
		t.Fatalf("429 on the tools request must stay transient with no retry: results=%+v err=%v calls=%d", got, err, calls.Load())
	}
}

func TestProbeToolsLessRetryTransient(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			p, srv := probeEnv(t, "retry-transient", func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("probe body: %v", err)
					return
				}
				if _, withTools := body["tools"]; withTools {
					http.Error(w, "tools unsupported", http.StatusBadRequest)
					return
				}
				http.Error(w, "retry later", status)
			})
			targets := []CheckTarget{{ID: "glm-5-2", Wire: "databricks-glm-5-2"}}
			got, err := Probe(context.Background(), p, targets, CheckOptions{Client: srv.Client(), ClientRoutes: true})
			if err != nil || len(got) != 1 {
				t.Fatalf("Probe = %+v, %v", got, err)
			}
			if got[0].Status != StatusTransient || got[0].HTTP != status {
				t.Errorf("tools-less retry = %+v, want transient HTTP %d", got[0], status)
			}
			if n := calls.Load(); n != 2 {
				t.Errorf("requests = %d, want tools request and tools-less retry", n)
			}
		})
	}
}

// A body read cut short mid-reply is a transport error, not a clean status.
func TestProbeRequestReportsTruncatedBody(t *testing.T) {
	vaulttest.Use(t)
	t.Setenv("CUT_TOKEN", "tok")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte("partial"))
	}))
	defer srv.Close()
	p := &providers.Provider{Name: "cut", Auth: providers.Auth{Type: providers.AuthKey, Env: "CUT_TOKEN"}}
	rep, err := probeRequest(context.Background(), p, CheckOptions{Client: srv.Client(), Timeout: 5 * time.Second}, ProbeCall{URL: srv.URL, Body: map[string]any{}})
	if err == nil || rep.HTTP != 200 {
		t.Errorf("status %d err %v, want 200 with a read error", rep.HTTP, err)
	}
}

// A non-2xx status stands even when its body is cut short, so the caller still sees the HTTP failure.
func TestProbeRequestKeepsNon2xxStatusOnTruncatedBody(t *testing.T) {
	vaulttest.Use(t)
	t.Setenv("CUT_TOKEN", "tok")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("partial"))
	}))
	defer srv.Close()
	p := &providers.Provider{Name: "cut", Auth: providers.Auth{Type: providers.AuthKey, Env: "CUT_TOKEN"}}
	rep, err := probeRequest(context.Background(), p, CheckOptions{Client: srv.Client(), Timeout: 5 * time.Second}, ProbeCall{URL: srv.URL, Body: map[string]any{}})
	if err != nil || rep.HTTP != 400 {
		t.Errorf("status %d err %v, want 400 with no error", rep.HTTP, err)
	}
}

// A probe timeout is named as such, and a DNS failure by its kind, never the bare "network error".
func TestProbeFailureNamesTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	if got := probeFailure(ctx, errors.New("x"), 15*time.Second); got.Error() != "timeout after 15s" {
		t.Errorf("probeFailure = %q", got)
	}
	if got := probeFailure(context.Background(), &net.DNSError{Name: "h"}, time.Second); got.Error() != "network error: DNS" {
		t.Errorf("DNS probeFailure = %q", got)
	}
}

type errBody struct{ err error }

func (b errBody) Read([]byte) (int, error) { return 0, b.err }
func (b errBody) Close() error             { return nil }

// A 2xx body read error must not echo the endpoint address from the net error text.
func TestReadBodyErrorDoesNotLeakAddress(t *testing.T) {
	resp := &http.Response{StatusCode: 200, Body: errBody{errors.New("read tcp 10.1.2.3:5->10.9.9.9:443: connection reset by peer")}}
	_, err := readBody(context.Background(), resp, time.Second)
	if err == nil || strings.Contains(err.Error(), "10.9.9.9") {
		t.Errorf("err = %v, want a non-nil error without the address", err)
	}
}

// A deadline firing during the 2xx body read reports the same timeout text as the Do-error path.
func TestReadBodyDeadlineReportsTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	resp := &http.Response{StatusCode: 200, Body: errBody{context.DeadlineExceeded}}
	_, err := readBody(ctx, resp, 5*time.Second)
	if err == nil || err.Error() != "timeout after 5s" {
		t.Errorf("err = %v, want timeout after 5s", err)
	}
}

// A reasoning model spends a tiny probe budget before its first output token; a healthy endpoint must not fail.
func TestProbeRetriesTokenBudgetExhausted(t *testing.T) {
	var calls atomic.Int32
	p, srv := probeEnv(t, "budget", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if n, _ := body["max_tokens"].(float64); n < 512 {
			http.Error(w, `{"error_code":"BAD_REQUEST","message":"Could not finish the message because max_tokens or model output limit was reached. Please try again with higher max_tokens."}`, http.StatusBadRequest)
			return
		}
		writeSSE(w, false, false, true)
	})
	targets := []CheckTarget{{ID: "gpt-5-1", Wire: "databricks-gpt-5-1"}}
	got, err := Probe(context.Background(), p, targets, CheckOptions{Client: srv.Client()})
	if err != nil || len(got) != 1 || got[0].Status != "ok" || got[0].Tools != "yes" || got[0].HTTP != 200 {
		t.Fatalf("budget-exhausted probe = %+v, %v; want ok with tools after one larger-budget retry", got, err)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("requests = %d, want the probe and one larger-budget retry", n)
	}
}

// writeSSE writes a short well-formed stream for the wire, with its terminator when term.
func writeSSE(w http.ResponseWriter, responses, anthropic, term bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	switch {
	case anthropic:
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\"}\n\n"))
		if term {
			_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
		}
	case responses:
		_, _ = w.Write([]byte("event: x\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\n"))
		if term {
			_, _ = w.Write([]byte("data: {\"type\":\"response.completed\"}\n\n"))
		}
	default:
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"OK\"}}]}\n\n"))
		if term {
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		}
	}
}

func probeStreamWire(t *testing.T, tc clientRouteCase, h func(w http.ResponseWriter, body map[string]any)) CheckResult {
	t.Helper()
	p, srv := probeEnv(t, "stream", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		h(w, body)
	})
	p.Protocol, p.BaseURL = providers.ProtocolAnthropic, srv.URL+"/serving-endpoints/custom"
	p.Routes = map[string]string{providers.ProtocolOpenAI: "chat", providers.ProtocolResponses: srv.URL + "/serving-endpoints/custom-responses"}
	tg := CheckTarget{ID: tc.id, Wire: "w-" + tc.id, Responses: tc.responses}
	got, err := Probe(context.Background(), p, []CheckTarget{tg}, CheckOptions{Client: srv.Client(), ClientRoutes: true})
	if err != nil || len(got) != 1 {
		t.Fatalf("Probe = %+v, %v", got, err)
	}
	return got[0]
}

func TestProbeStreamPerWire(t *testing.T) {
	for _, tc := range clientRouteCases[:3] {
		t.Run(tc.name+"/complete", func(t *testing.T) {
			r := probeStreamWire(t, tc, func(w http.ResponseWriter, body map[string]any) {
				if body["stream"] != true {
					t.Errorf("stream = %v, want true", body["stream"])
				}
				writeSSE(w, tc.responses, tc.anthropic, true)
			})
			if r.Status != StatusOK {
				t.Errorf("well-formed stream = %+v, want ok", r)
			}
		})
		t.Run(tc.name+"/no terminator", func(t *testing.T) {
			r := probeStreamWire(t, tc, func(w http.ResponseWriter, _ map[string]any) { writeSSE(w, tc.responses, tc.anthropic, false) })
			if r.Status != StatusBroken || r.Events != 1 || r.Bytes == 0 || r.HTTP != 200 {
				t.Errorf("unterminated stream = %+v, want broken with 1 event", r)
			}
		})
		t.Run(tc.name+"/cut", func(t *testing.T) {
			r := probeStreamWire(t, tc, func(w http.ResponseWriter, _ map[string]any) {
				w.Header().Set("Content-Length", "4096") // the server closes short: an unexpected EOF
				writeSSE(w, tc.responses, tc.anthropic, false)
			})
			if r.Status != StatusBroken || r.Events != 1 {
				t.Errorf("cut stream = %+v, want broken with 1 event", r)
			}
		})
	}
}

func TestProbeStreamTerminatorWithoutContentIsBroken(t *testing.T) {
	r := probeStreamWire(t, clientRouteCases[0], func(w http.ResponseWriter, _ map[string]any) {
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})
	if r.Status != StatusBroken || r.Events != 0 {
		t.Errorf("terminator only = %+v, want broken", r)
	}
}

func TestProbeStreamDeprecatedJSONStaysFail(t *testing.T) {
	r := probeStreamWire(t, clientRouteCases[0], func(w http.ResponseWriter, _ map[string]any) {
		http.Error(w, `{"message":"This endpoint w-gpt-5-1 is deprecated"}`, http.StatusBadRequest)
	})
	if r.Status != StatusFail || r.HTTP != 400 || !strings.Contains(r.Error, "is deprecated") {
		t.Errorf("deprecated reply = %+v, want fail 400 deprecated", r)
	}
}

func TestProbeNoStreamTargetSendsPlainRequest(t *testing.T) {
	r := probeStreamWire(t, clientRouteCases[0], func(w http.ResponseWriter, body map[string]any) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	if r.Status != StatusBroken {
		t.Fatalf("control: a streamed probe of a plain JSON reply = %+v, want broken", r)
	}
	p, srv := probeEnv(t, "plain", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if _, ok := body["stream"]; ok {
			t.Errorf("non-chat target sent stream: %v", body)
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	tg := CheckTarget{ID: "embed", Wire: "embed", NoStream: true}
	got, err := Probe(context.Background(), p, []CheckTarget{tg}, CheckOptions{Client: srv.Client(), ClientRoutes: true})
	if err != nil || len(got) != 1 || got[0].Status != StatusOK {
		t.Errorf("non-streaming target = %+v, %v, want ok", got, err)
	}
}

// Databricks chat streams may end after the finish_reason chunk with no [DONE]; that is a whole stream.
func TestProbeStreamChatFinishReasonWithoutDoneIsOK(t *testing.T) {
	r := probeStreamWire(t, clientRouteCases[0], func(w http.ResponseWriter, _ map[string]any) {
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"OK\"}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"))
	})
	if r.Status != StatusOK {
		t.Errorf("finish_reason stream = %+v, want ok", r)
	}
}

func TestProbeStreamErrorFrameIsBroken(t *testing.T) {
	r := probeStreamWire(t, clientRouteCases[0], func(w http.ResponseWriter, _ map[string]any) {
		_, _ = w.Write([]byte("data: {\"error\":{\"message\":\"upstream failed\"},\"choices\":[{\"delta\":{}}]}\n\ndata: [DONE]\n\n"))
	})
	if r.Status != StatusBroken {
		t.Errorf("error frame then [DONE] = %+v, want broken", r)
	}
}

func TestSanitizeChecksKeepsBrokenVerdict(t *testing.T) {
	got := SanitizeChecks([]CheckResult{{ID: "m", Status: StatusBroken, HTTP: 200, Error: "stream ended"}})
	if len(got) != 1 || got[0].Status != StatusBroken || got[0].Error != "" {
		t.Errorf("SanitizeChecks = %+v, want the broken verdict kept without its error text", got)
	}
}

func TestDeprecatedEndpointNeedsTheExplicitAnswer(t *testing.T) {
	const body = `{"message":"BAD_REQUEST: This endpoint databricks-gpt-5 is deprecated. See docs."}`
	if id, ok := DeprecatedEndpoint(400, body); !ok || id != "databricks-gpt-5" {
		t.Errorf("400 = %q %v, want databricks-gpt-5", id, ok)
	}
	for _, c := range []struct {
		code int
		body string
	}{{429, body}, {503, body}, {200, body}, {401, body}, {400, `{"message":"bad request shape"}`}} {
		if _, ok := DeprecatedEndpoint(c.code, c.body); ok {
			t.Errorf("HTTP %d %q qualified, want not", c.code, c.body)
		}
	}
}
