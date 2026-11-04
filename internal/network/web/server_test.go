package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/network/browser"
	"github.com/edotau/claude-code/internal/network/tailscale"
)

// agenttreeTestdata is internal/agent's own testdata fixture dir, reused rather than duplicated.
const agenttreeTestdata = "../../agent/tree/testdata"

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
		opts:       opts,
		boundAddr:  "127.0.0.1:7777",
		dnsNames:   []string{"my-node.tailxyz.ts.net", "my-node"},
		csrfToken:  testCSRFToken,
		logger:     log.New(logBuf, "", 0),
		status:     newStatusCache(),
		now:        time.Now,
		browserEnv: browser.NewEnv(), // tests that exercise /api/browsers override this via newBrowserTestServer
		browsers:   newBrowserCache(),
	}
	s.mux = http.NewServeMux()
	s.routes()
	return s
}

// newStatusTestServer is newTestServer plus an injected fake Runner and clock, for handleDispatches
// tests that must never touch a real ssh/tmux and must control cache expiry deterministically.
func newStatusTestServer(t *testing.T, opts Options, run tailscale.RemoteRunner, now func() time.Time) *Server {
	t.Helper()
	s := newTestServer(t, opts, nil)
	s.run = run
	s.now = now
	return s
}

// countingCaptureRunner returns out/err for every call and counts how many times it was invoked, to
// assert on cache-hit vs. cache-miss behavior without a real ssh/tmux round trip.
func countingCaptureRunner(out string, err error) (tailscale.RemoteRunner, *int) {
	n := 0
	return func(context.Context, []string) ([]byte, error) {
		n++
		return []byte(out), err
	}, &n
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

func TestRunCanceledContextShutsDownCleanly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := Run(ctx, Options{Addr: "127.0.0.1:0", ErrOut: io.Discard})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil after clean shutdown", err)
	}
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

func TestGuardAndOriginOnTree(t *testing.T) {
	cases := []struct {
		name   string
		host   string
		header [2]string
		want   int
	}{
		{"bad host", "evil.example", [2]string{}, http.StatusMisdirectedRequest},
		{"missing host", "", [2]string{}, http.StatusMisdirectedRequest},
		{"bound host", "127.0.0.1:7777", [2]string{}, http.StatusOK},
		{"sec-fetch-site cross-site", "127.0.0.1:7777", [2]string{"Sec-Fetch-Site", "cross-site"}, http.StatusForbidden},
		{"sec-fetch-site same-origin", "127.0.0.1:7777", [2]string{"Sec-Fetch-Site", "same-origin"}, http.StatusOK},
		{"cross-origin Origin", "127.0.0.1:7777", [2]string{"Origin", "http://evil.example"}, http.StatusForbidden},
		{"same-origin Origin", "127.0.0.1:7777", [2]string{"Origin", "http://127.0.0.1:7777"}, http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newTestServer(t, Options{Days: 36500}, nil)
			req := newReq(t, "GET", "/api/tree", c.host)
			if c.header[0] != "" {
				req.Header.Set(c.header[0], c.header[1])
			}
			if w := do(s, req); w.Code != c.want {
				t.Fatalf("status = %d, want %d; body=%s", w.Code, c.want, w.Body.String())
			}
		})
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

// postDispatchRaw posts a pre-built body with an explicit Content-Type, for cases dispatchForm's
// url.Values.Encode (which can't produce a worst-case 3x-expanded percent-encoding) can't build.
func postDispatchRaw(t *testing.T, s *Server, token, contentType, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/dispatch", strings.NewReader(body))
	req.Host = "127.0.0.1:7777"
	req.Header.Set("Content-Type", contentType)
	if token != "" {
		req.Header.Set(csrfHeader, token)
	}
	return do(s, req)
}

// multipartForm encodes fields as a multipart body and returns its content type and payload.
func multipartForm(t *testing.T, fields map[string]string) (contentType, body string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for field, val := range fields {
		if err := mw.WriteField(field, val); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return mw.FormDataContentType(), buf.String()
}

// A form or multipart body over maxBodyBytes must be rejected before any field is even decoded.
func TestDispatchBodyCapForm(t *testing.T) {
	huge := strings.Repeat("x", int(maxBodyBytes)+1000)
	s := newTestServer(t, Options{DryRunDispatch: true}, nil)
	form := dispatchForm("localhost", "claude", huge, "hi")
	w := postDispatch(t, s, testCSRFToken, form)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("oversize form body: status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestDispatchBodyCapMultipart(t *testing.T) {
	huge := strings.Repeat("x", int(maxBodyBytes)+1000)
	s := newTestServer(t, Options{DryRunDispatch: true}, nil)
	ctype, body := multipartForm(t, map[string]string{"host": "localhost", "agent": "claude", "dir": huge, "task": "hi"})
	w := postDispatchRaw(t, s, testCSRFToken, ctype, body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("oversize multipart body: status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestDispatchBodyCapMultipartAccepted(t *testing.T) {
	s := newTestServer(t, Options{DryRunDispatch: true}, nil)
	ctype, body := multipartForm(t, map[string]string{"host": "localhost", "agent": "claude", "task": "hi"})
	w := postDispatchRaw(t, s, testCSRFToken, ctype, body)
	if w.Code != http.StatusOK {
		t.Fatalf("multipart dispatch: status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// An 8KiB all-space task, percent-encoded %20 (3x expansion, the worst case maxBodyBytes sizes
// for), stays within the body cap and must still be accepted.
func TestDispatchBodyCapWorstCaseEncodingAccepted(t *testing.T) {
	s := newTestServer(t, Options{DryRunDispatch: true}, nil)
	body := "host=localhost&agent=claude&dir=&task=" + strings.Repeat("%20", maxTaskBytes)
	w := postDispatchRaw(t, s, testCSRFToken, "application/x-www-form-urlencoded", body)
	if w.Code != http.StatusOK {
		t.Fatalf("worst-case-encoded 8KiB task: status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// resolveHosts must return opts.Hosts verbatim, every call, without ever touching the tailnet cache.
func TestResolveHostsCached(t *testing.T) {
	s := newTestServer(t, Options{Hosts: []string{"box-a", "box-b"}}, nil)
	first := s.resolveHosts(context.Background())
	second := s.resolveHosts(context.Background())
	if !slices.Equal(first, s.opts.Hosts) || !slices.Equal(second, s.opts.Hosts) {
		t.Fatalf("resolveHosts() = %v / %v, want opts.Hosts verbatim %v", first, second, s.opts.Hosts)
	}
	if !s.hosts.sampled.IsZero() {
		t.Error("resolveHosts populated the tailnet cache although opts.Hosts was set")
	}
}

func TestDispatchMissingFieldsRejected(t *testing.T) {
	s := newTestServer(t, Options{DryRunDispatch: true}, nil)
	w := postDispatch(t, s, testCSRFToken, dispatchForm("", "claude", "", "hello"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing host: status = %d, want 400", w.Code)
	}
}

// --- hostile task reaches dry-run argv as the same quoted payload tailscale.BuildRemoteCommand builds,
// and nothing executes (dry-run never calls exec.Command) ---

// checkHostileDryRun posts task as a dry-run dispatch and asserts the argv equals dispatch's own builder output.
func checkHostileDryRun(t *testing.T, task string) {
	t.Helper()
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
	wantCmd, wantName, err := tailscale.BuildRemoteCommand(tailscale.RemoteOpts{
		Agent: "claude", Task: task, Name: resp.Session,
	})
	if err != nil {
		t.Fatalf("BuildRemoteCommand: %v", err)
	}
	if wantName != resp.Session {
		t.Fatalf("name mismatch: got %q want %q", resp.Session, wantName)
	}
	wantArgv := tailscale.ExecArgs(context.Background(), "localhost", wantCmd, true) // localhost runs locally, no ssh
	if len(resp.Argv) != len(wantArgv) {
		t.Fatalf("argv length = %d, want %d: got=%v want=%v", len(resp.Argv), len(wantArgv), resp.Argv, wantArgv)
	}
	for i := range wantArgv {
		if resp.Argv[i] != wantArgv[i] {
			t.Errorf("argv[%d] = %q, want %q", i, resp.Argv[i], wantArgv[i])
		}
	}
}

// Byte-identical argv is the authoritative proof the hostile payload went through the same QuoteSH escaping
// dispatch itself uses, with no extra interpolation from the web layer; --dry-run-dispatch never execs.
func TestDispatchHostileTaskDryRunMatchesBuildRemoteCommand(t *testing.T) {
	hostileTasks := []string{
		`'; touch /tmp/x; '`,
		"$(id)",
		"line one\nline two\n; rm -rf /",
		"`whoami`",
	}
	for _, task := range hostileTasks {
		t.Run(task, func(t *testing.T) { checkHostileDryRun(t, task) })
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

// --- /api/tree returns valid JSON with the agent-tree schema ---

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

// --- /api/dispatches: classified status only, expiring cache ---

// webCaptureFrame builds one captureCommand-shaped record matching dispatch's on-wire framing, so
// these tests exercise the real parseCaptureBlob/Observe/Classify pipeline through a fake Runner.
func webCaptureFrame(sess, pid, agent, cmd, text string) string {
	return "\x01" + sess + "|" + pid + "|" + agent + "|" + cmd + "\n" + text + "\x02\n"
}

func TestHandleDispatchesReturnsClassifiedStatusNotRawCaptureText(t *testing.T) {
	secretText := "raw pane contents: <script>alert(document.cookie)</script> should never leave the server"
	run, _ := countingCaptureRunner(webCaptureFrame("cc-claude-a", "%1", "claude", "claude", secretText), nil)
	s := newStatusTestServer(t, Options{}, run, time.Now)
	req := newReq(t, "GET", "/api/dispatches?host=localhost", "127.0.0.1:7777")
	w := do(s, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, secretText) {
		t.Fatalf("/api/dispatches leaked raw pane capture text: %s", body)
	}
	var statuses []tailscale.PaneStatus
	if err := json.Unmarshal(w.Body.Bytes(), &statuses); err != nil {
		t.Fatalf("decode /api/dispatches body: %v; body=%s", err, body)
	}
	if len(statuses) != 1 || statuses[0].Session != "cc-claude-a" || statuses[0].State == "" {
		t.Fatalf("statuses = %+v, want one classified cc-claude-a status", statuses)
	}
}

func TestHandleDispatchesUnknownHostRejected(t *testing.T) {
	run, calls := countingCaptureRunner("", nil)
	s := newStatusTestServer(t, Options{Hosts: []string{"box-a"}}, run, time.Now)
	req := newReq(t, "GET", "/api/dispatches?host=box-does-not-exist", "127.0.0.1:7777")
	w := do(s, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown host: status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if *calls != 0 {
		t.Errorf("an unknown host must never reach the capture runner, got %d calls", *calls)
	}
}

func TestHandleDispatchesMissingHostIsBadRequest(t *testing.T) {
	s := newTestServer(t, Options{}, nil)
	req := newReq(t, "GET", "/api/dispatches", "127.0.0.1:7777")
	w := do(s, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing ?host: status = %d, want 400", w.Code)
	}
}

func TestHandleDispatchesServesWithinTTLFromCacheWithoutResampling(t *testing.T) {
	run, calls := countingCaptureRunner(webCaptureFrame("cc-claude-a", "%1", "claude", "claude", "$ "), nil)
	fakeNow := time.Now()
	s := newStatusTestServer(t, Options{}, run, func() time.Time { return fakeNow })

	for i := range 3 {
		w := do(s, newReq(t, "GET", "/api/dispatches?host=localhost", "127.0.0.1:7777"))
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i, w.Code)
		}
	}
	if *calls != 1 {
		t.Errorf("3 requests within the cache TTL should sample once, got %d capture calls", *calls)
	}
}

func TestHandleDispatchesStaleCacheExpiresAndResamples(t *testing.T) {
	run, calls := countingCaptureRunner(webCaptureFrame("cc-claude-a", "%1", "claude", "claude", "$ "), nil)
	fakeNow := time.Now()
	s := newStatusTestServer(t, Options{}, run, func() time.Time { return fakeNow })

	w1 := do(s, newReq(t, "GET", "/api/dispatches?host=localhost", "127.0.0.1:7777"))
	if w1.Code != http.StatusOK {
		t.Fatalf("first request: status = %d, want 200", w1.Code)
	}
	// Advance the fake clock past the cache TTL: the next request must trigger a fresh capture.
	fakeNow = fakeNow.Add(statusCacheTTL + time.Second)
	w2 := do(s, newReq(t, "GET", "/api/dispatches?host=localhost", "127.0.0.1:7777"))
	if w2.Code != http.StatusOK {
		t.Fatalf("second request (post-expiry): status = %d, want 200", w2.Code)
	}
	if *calls != 2 {
		t.Errorf("a request past the cache TTL should resample, got %d capture calls, want 2", *calls)
	}
}

func TestHandleDispatchesCaptureFailureIsBadGatewayWhenNothingCached(t *testing.T) {
	run, _ := countingCaptureRunner("", errSimulatedSSH)
	s := newStatusTestServer(t, Options{}, run, time.Now)
	w := do(s, newReq(t, "GET", "/api/dispatches?host=localhost", "127.0.0.1:7777"))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("capture failure with nothing cached yet: status = %d, want 502; body=%s", w.Code, w.Body.String())
	}
}

type simulatedSSHErr string

func (e simulatedSSHErr) Error() string { return string(e) }

var errSimulatedSSH = simulatedSSHErr("ssh: connection refused")

func TestValidateBindAddr(t *testing.T) {
	cases := []struct {
		addr string
		ok   bool
	}{
		{"127.0.0.1:7777", true},
		{"[::1]:7777", true},
		{"100.101.102.103:7777", true}, // tailnet CGNAT range
		{"0.0.0.0:7777", false},
		{"[::]:7777", false},
		{"192.168.1.5:7777", false},
		{"8.8.8.8:7777", false},
		{":7777", false},          // empty host == every interface
		{"localhost:7777", false}, // hostname, not a literal IP
		{"127.0.0.1:abc", false},  // bad port
		{"127.0.0.1", false},      // missing port
	}
	for _, c := range cases {
		ip, err := validateBindAddr(c.addr)
		if c.ok && err != nil {
			t.Errorf("validateBindAddr(%q) = err %v, want ok", c.addr, err)
		}
		if !c.ok && err == nil {
			t.Errorf("validateBindAddr(%q) = ip %v, want error", c.addr, ip)
		}
	}
}

func TestIsCGNAT(t *testing.T) {
	cases := []struct {
		ip string
		ok bool
	}{
		{"100.64.0.1", true},
		{"100.127.255.255", true},
		{"100.63.255.255", false},
		{"100.128.0.0", false},
		{"10.0.0.1", false},
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		if ip == nil {
			t.Fatalf("net.ParseIP(%q) = nil", c.ip)
		}
		if got := isCGNAT(ip); got != c.ok {
			t.Errorf("isCGNAT(%q) = %v, want %v", c.ip, got, c.ok)
		}
	}
}

func tickReq(t *testing.T, token string, body any) *http.Request {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/api/plan/tick", strings.NewReader(string(b)))
	req.Host = "127.0.0.1:7777"
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set(csrfHeader, token)
	}
	return req
}

const tickTitle = "1.2 Open owns: b.go"

func tickBody(path, title string) map[string]any {
	return map[string]any{"path": path, "line": 6, "title": title, "done": true}
}

// tickServer is a plans server whose clock is fixed, plus the old plan's relative and full path.
func tickServer(t *testing.T) (s *Server, rel, full string) {
	t.Helper()
	s, dir := plansServer(t)
	fixed := time.Date(2026, 10, 31, 12, 30, 0, 0, time.UTC)
	s.now = func() time.Time { return fixed }
	rel = "2026-10-01-old.md"
	return s, rel, filepath.Join(dir, "plans", rel)
}

func readFileString(full string) string { b, _ := os.ReadFile(full); return string(b) }

// The endpoint answers 403/409/400 on bad requests and leaves the plan alone on each.
func TestPlanTickEndpointRejectsBadRequests(t *testing.T) {
	s, rel, full := tickServer(t)
	orig := readFileString(full)

	if w := do(s, tickReq(t, "", tickBody(rel, tickTitle))); w.Code != http.StatusForbidden {
		t.Fatalf("no CSRF = %d, want 403", w.Code)
	}
	if w := do(s, tickReq(t, testCSRFToken, tickBody(rel, "9.9 Nope"))); w.Code != http.StatusConflict {
		t.Fatalf("wrong title = %d %s, want 409", w.Code, w.Body)
	}
	for _, p := range []string{"../settings.md", "../../etc/passwd"} {
		if w := do(s, tickReq(t, testCSRFToken, tickBody(p, tickTitle))); w.Code != http.StatusBadRequest {
			t.Fatalf("path %q = %d, want 400", p, w.Code)
		}
	}
	if got := readFileString(full); got != orig {
		t.Fatalf("rejected requests changed the plan:\n%s", got)
	}
}

// The endpoint ticks with s.now() and keeps the working claim.
func TestPlanTickEndpointTicks(t *testing.T) {
	s, rel, full := tickServer(t)
	w := do(s, tickReq(t, testCSRFToken, tickBody(rel, tickTitle)))
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"ok":true}` {
		t.Fatalf("tick = %d %s", w.Code, w.Body)
	}
	got := readFileString(full)
	if !strings.Contains(got, "- [x] 1.2 Open") || !strings.Contains(got, "<!-- user: ticked 2026-10-31T12:30:00Z -->") ||
		!strings.Contains(got, "<!-- working: bob -->") {
		t.Fatalf("plan after tick:\n%s", got)
	}
}
