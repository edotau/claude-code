package web

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/dispatch"
)

// agenttreeTestdata is internal/agenttree's own testdata fixture dir, reused rather than duplicated.
const agenttreeTestdata = "../agenttree/testdata"

const testCSRFToken = "test-csrf-token-0123456789abcdef"

// newTestServer builds a Server directly (bypassing New's live tailscale lookups) with fixed,
// deterministic fields so handler/guard behavior can be tested without a real tailnet.
func newTestServer(t *testing.T, opts Options, logBuf *bytes.Buffer) *Server {
	t.Helper()
	if opts.ClaudeDir == "" {
		opts.ClaudeDir = agenttreeTestdata
	}
	if logBuf == nil {
		logBuf = &bytes.Buffer{}
	}
	s := &Server{
		opts:      opts,
		boundAddr: "127.0.0.1:7777",
		dnsNames:  []string{"my-node.tailxyz.ts.net", "my-node"},
		csrfToken: testCSRFToken,
		logger:    log.New(logBuf, "", 0),
	}
	s.mux = http.NewServeMux()
	s.routes()
	return s
}

func do(s *Server, req *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	return w
}

func newReq(t *testing.T, method, target, host string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	req.Host = host
	return req
}

// --- Host header allow/deny ---

func TestHostAllowed(t *testing.T) {
	s := newTestServer(t, Options{}, nil)
	cases := []struct {
		name string
		host string
		want bool
	}{
		{"exact bound addr", "127.0.0.1:7777", true},
		{"localhost no port", "localhost", true},
		{"localhost with port", "localhost:7777", true},
		{"dns name full", "my-node.tailxyz.ts.net", true},
		{"dns name short", "my-node", true},
		{"dns name with port", "my-node:7777", true},
		{"evil public dns", "evil.example", false},
		{"missing host", "", false},
		{"ip port mismatch", "127.0.0.1:9999", false},
		{"different loopback port, no port stripped match", "127.0.0.1:1", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := s.hostAllowed(c.host); got != c.want {
				t.Errorf("hostAllowed(%q) = %v, want %v", c.host, got, c.want)
			}
		})
	}
}

func TestGuardRejectsBadHost(t *testing.T) {
	s := newTestServer(t, Options{}, nil)
	req := newReq(t, "GET", "/api/tree", "evil.example")
	w := do(s, req)
	if w.Code != http.StatusMisdirectedRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusMisdirectedRequest)
	}
}

func TestGuardRejectsMissingHost(t *testing.T) {
	s := newTestServer(t, Options{}, nil)
	req := newReq(t, "GET", "/api/tree", "")
	w := do(s, req)
	if w.Code != http.StatusMisdirectedRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusMisdirectedRequest)
	}
}

func TestGuardAllowsBoundHost(t *testing.T) {
	s := newTestServer(t, Options{Days: 36500}, nil)
	req := newReq(t, "GET", "/api/tree", "127.0.0.1:7777")
	w := do(s, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// --- Origin / Sec-Fetch-Site cross-origin rejection ---

func TestCheckOriginCrossSite(t *testing.T) {
	s := newTestServer(t, Options{}, nil)
	req := newReq(t, "GET", "/api/tree", "127.0.0.1:7777")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	w := do(s, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("Sec-Fetch-Site cross-site: status = %d, want 403", w.Code)
	}
}

func TestCheckOriginSameOriginAllowed(t *testing.T) {
	s := newTestServer(t, Options{Days: 36500}, nil)
	req := newReq(t, "GET", "/api/tree", "127.0.0.1:7777")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	w := do(s, req)
	if w.Code != http.StatusOK {
		t.Fatalf("Sec-Fetch-Site same-origin: status = %d, want 200", w.Code)
	}
}

func TestCheckOriginCrossOriginHeaderRejected(t *testing.T) {
	s := newTestServer(t, Options{}, nil)
	req := newReq(t, "GET", "/api/tree", "127.0.0.1:7777")
	req.Header.Set("Origin", "http://evil.example")
	w := do(s, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin Origin: status = %d, want 403", w.Code)
	}
}

func TestCheckOriginSameOriginHeaderAllowed(t *testing.T) {
	s := newTestServer(t, Options{Days: 36500}, nil)
	req := newReq(t, "GET", "/api/tree", "127.0.0.1:7777")
	req.Header.Set("Origin", "http://127.0.0.1:7777")
	w := do(s, req)
	if w.Code != http.StatusOK {
		t.Fatalf("same-origin Origin: status = %d, want 200", w.Code)
	}
}

// --- CSRF ---

func dispatchForm(host, agent, dir, task string) *url.Values {
	v := &url.Values{}
	v.Set("host", host)
	v.Set("agent", agent)
	v.Set("dir", dir)
	v.Set("task", task)
	return v
}

func postDispatch(t *testing.T, s *Server, token string, form *url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/dispatch", strings.NewReader(form.Encode()))
	req.Host = "127.0.0.1:7777"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if token != "" {
		req.Header.Set(csrfHeader, token)
	}
	return do(s, req)
}

func TestDispatchMissingCSRF(t *testing.T) {
	s := newTestServer(t, Options{DryRunDispatch: true}, nil)
	w := postDispatch(t, s, "", dispatchForm("localhost", "claude", "", "hello"))
	if w.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF: status = %d, want 403", w.Code)
	}
}

func TestDispatchWrongCSRF(t *testing.T) {
	s := newTestServer(t, Options{DryRunDispatch: true}, nil)
	w := postDispatch(t, s, "not-the-token", dispatchForm("localhost", "claude", "", "hello"))
	if w.Code != http.StatusForbidden {
		t.Fatalf("wrong CSRF: status = %d, want 403", w.Code)
	}
}

func TestDispatchCorrectCSRF(t *testing.T) {
	s := newTestServer(t, Options{DryRunDispatch: true}, nil)
	w := postDispatch(t, s, testCSRFToken, dispatchForm("localhost", "claude", "", "hello"))
	if w.Code != http.StatusOK {
		t.Fatalf("correct CSRF: status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// --- method not allowed ---

// The index route is registered as "GET /{$}" (exact-root only), not the "GET /" wildcard — without
// that, net/http.ServeMux treats "GET /" as a catch-all for every unmatched GET path, and a GET to
// /api/dispatch would silently fall through to handleIndex (200) instead of 405.
func TestDispatchGETMethodNotAllowed(t *testing.T) {
	s := newTestServer(t, Options{}, nil)
	req := newReq(t, "GET", "/api/dispatch", "127.0.0.1:7777")
	w := do(s, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /api/dispatch: status = %d, want 405", w.Code)
	}
}

// --- host allowlist / agent allowlist / task size on dispatch body ---

func TestDispatchUnknownHostRejected(t *testing.T) {
	s := newTestServer(t, Options{DryRunDispatch: true, Hosts: []string{"box-a"}}, nil)
	w := postDispatch(t, s, testCSRFToken, dispatchForm("box-does-not-exist", "claude", "", "hello"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown host: status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestDispatchUnknownAgentRejected(t *testing.T) {
	s := newTestServer(t, Options{DryRunDispatch: true}, nil)
	w := postDispatch(t, s, testCSRFToken, dispatchForm("localhost", "not-a-real-agent", "", "hello"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown agent: status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestDispatchOversizeTaskRejected(t *testing.T) {
	s := newTestServer(t, Options{DryRunDispatch: true}, nil)
	task := strings.Repeat("a", maxTaskBytes+1)
	w := postDispatch(t, s, testCSRFToken, dispatchForm("localhost", "claude", "", task))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("oversize task: status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestDispatchMissingFieldsRejected(t *testing.T) {
	s := newTestServer(t, Options{DryRunDispatch: true}, nil)
	w := postDispatch(t, s, testCSRFToken, dispatchForm("", "claude", "", "hello"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing host: status = %d, want 400", w.Code)
	}
}

// --- hostile task reaches dry-run argv as the same quoted payload dispatch.BuildRemoteCommand builds,
// and nothing executes (dry-run never calls exec.Command) ---

func TestDispatchHostileTaskDryRunMatchesBuildRemoteCommand(t *testing.T) {
	hostileTasks := []string{
		`'; touch /tmp/x; '`,
		"$(id)",
		"line one\nline two\n; rm -rf /",
		"`whoami`",
	}
	for _, task := range hostileTasks {
		t.Run(task, func(t *testing.T) {
			s := newTestServer(t, Options{DryRunDispatch: true}, nil)
			w := postDispatch(t, s, testCSRFToken, dispatchForm("localhost", "claude", "", task))
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
			}
			var resp dispatchResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode response: %v; body=%s", err, w.Body.String())
			}
			if resp.Session == "" || len(resp.Argv) == 0 {
				t.Fatalf("dry-run response missing session/argv: %+v", resp)
			}
			// Rebuild the exact same remote command using the session name the handler generated,
			// and confirm the handler's dry-run argv is byte-identical to dispatch's own builder.
			wantCmd, wantName, err := dispatch.BuildRemoteCommand(dispatch.Opts{
				Agent: "claude", Task: task, Name: resp.Session,
			})
			if err != nil {
				t.Fatalf("BuildRemoteCommand: %v", err)
			}
			if wantName != resp.Session {
				t.Fatalf("name mismatch: got %q want %q", resp.Session, wantName)
			}
			wantArgv := dispatch.SSHArgs("localhost", wantCmd)
			if len(resp.Argv) != len(wantArgv) {
				t.Fatalf("argv length = %d, want %d: got=%v want=%v", len(resp.Argv), len(wantArgv), resp.Argv, wantArgv)
			}
			for i := range wantArgv {
				if resp.Argv[i] != wantArgv[i] {
					t.Errorf("argv[%d] = %q, want %q", i, resp.Argv[i], wantArgv[i])
				}
			}
			// Byte-identical to dispatch's own builder (checked above) is the authoritative proof
			// the hostile payload went through the same QuoteSH escaping dispatch itself uses, with
			// no extra interpolation from the web layer. Nothing executes either way: --dry-run-dispatch
			// returns the argv instead of calling exec.Command.
		})
	}
}

// --- GET / contains the CSRF token and no external asset URLs ---

func TestHandleIndexContainsTokenNoExternalAssets(t *testing.T) {
	s := newTestServer(t, Options{}, nil)
	req := newReq(t, "GET", "/", "127.0.0.1:7777")
	w := do(s, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /: status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, testCSRFToken) {
		t.Errorf("GET / body does not contain the CSRF token")
	}
	for _, attr := range []string{"src=\"http://", "src=\"https://", "href=\"http://", "href=\"https://", "src='http://", "src='https://"} {
		if strings.Contains(body, attr) {
			t.Errorf("GET / body contains an external asset reference %q", attr)
		}
	}
}

// --- /api/tree returns valid JSON with the agenttree schema ---

func TestHandleTreeReturnsAgenttreeSchema(t *testing.T) {
	s := newTestServer(t, Options{Days: 36500}, nil)
	req := newReq(t, "GET", "/api/tree", "127.0.0.1:7777")
	w := do(s, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/tree: status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var payload struct {
		Sessions []map[string]any `json:"sessions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode /api/tree body: %v; body=%s", err, w.Body.String())
	}
	if len(payload.Sessions) == 0 {
		t.Fatalf("/api/tree returned no sessions from fixture %s; body=%s", agenttreeTestdata, w.Body.String())
	}
	for _, key := range []string{"id", "project", "cwd", "status", "tokens", "subagents"} {
		if _, ok := payload.Sessions[0][key]; !ok {
			t.Errorf("session missing %q field: %+v", key, payload.Sessions[0])
		}
	}
}

// --- access log contains no request body / CSRF token ---

func TestAccessLogExcludesBodyAndToken(t *testing.T) {
	var logBuf bytes.Buffer
	s := newTestServer(t, Options{DryRunDispatch: true}, &logBuf)
	secretTask := "super-secret-task-payload-should-not-be-logged"
	w := postDispatch(t, s, testCSRFToken, dispatchForm("localhost", "claude", "", secretTask))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	logged := logBuf.String()
	if strings.Contains(logged, secretTask) {
		t.Errorf("access log contains the request body/task: %s", logged)
	}
	if strings.Contains(logged, testCSRFToken) {
		t.Errorf("access log contains the CSRF token: %s", logged)
	}
}
