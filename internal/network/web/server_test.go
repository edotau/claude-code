package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/tree"
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

// A form or multipart body over maxBodyBytes must be rejected before any field is even decoded.
func TestDispatchBodyCap(t *testing.T) {
	huge := strings.Repeat("x", int(maxBodyBytes)+1000)

	t.Run("form", func(t *testing.T) {
		s := newTestServer(t, Options{DryRunDispatch: true}, nil)
		form := dispatchForm("localhost", "claude", huge, "hi")
		w := postDispatch(t, s, testCSRFToken, form)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("oversize form body: status = %d, want 400; body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("multipart", func(t *testing.T) {
		s := newTestServer(t, Options{DryRunDispatch: true}, nil)
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		for field, val := range map[string]string{"host": "localhost", "agent": "claude", "dir": huge, "task": "hi"} {
			if err := mw.WriteField(field, val); err != nil {
				t.Fatal(err)
			}
		}
		if err := mw.Close(); err != nil {
			t.Fatal(err)
		}
		w := postDispatchRaw(t, s, testCSRFToken, mw.FormDataContentType(), buf.String())
		if w.Code != http.StatusBadRequest {
			t.Fatalf("oversize multipart body: status = %d, want 400; body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("multipart-accepted", func(t *testing.T) {
		s := newTestServer(t, Options{DryRunDispatch: true}, nil)
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		for field, val := range map[string]string{"host": "localhost", "agent": "claude", "task": "hi"} {
			_ = mw.WriteField(field, val)
		}
		_ = mw.Close()
		w := postDispatchRaw(t, s, testCSRFToken, mw.FormDataContentType(), buf.String())
		if w.Code != http.StatusOK {
			t.Fatalf("multipart dispatch: status = %d, want 200; body=%s", w.Code, w.Body.String())
		}
	})

	// An 8KiB all-space task, percent-encoded %20 (3x expansion, the worst case maxBodyBytes sizes
	// for), stays within the body cap and must still be accepted.
	t.Run("worst-case-encoding-accepted", func(t *testing.T) {
		s := newTestServer(t, Options{DryRunDispatch: true}, nil)
		body := "host=localhost&agent=claude&dir=&task=" + strings.Repeat("%20", maxTaskBytes)
		w := postDispatchRaw(t, s, testCSRFToken, "application/x-www-form-urlencoded", body)
		if w.Code != http.StatusOK {
			t.Fatalf("worst-case-encoded 8KiB task: status = %d, want 200; body=%s", w.Code, w.Body.String())
		}
	})
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

// --- dashboard template escapes pane status fields before interpolating them into HTML ---

func TestRenderDispatchesTemplateEscapesStatusFields(t *testing.T) {
	src, err := assetsFS.ReadFile("assets/index.html.tmpl")
	if err != nil {
		t.Fatalf("read index.html.tmpl: %v", err)
	}
	body := string(src)
	start := strings.Index(body, "function renderDispatches")
	if start < 0 {
		t.Fatal("renderDispatches function not found in the dashboard template")
	}
	end := strings.Index(body[start:], "\n}\n")
	if end < 0 {
		t.Fatal("could not find the end of renderDispatches")
	}
	fn := body[start : start+end]
	for _, field := range []string{"p.session", "p.agent", "p.reason"} {
		if !strings.Contains(fn, "escapeHTML("+field) {
			t.Errorf("renderDispatches interpolates %s without escapeHTML(...): regression risk for a pane running attacker-controlled text\n%s", field, fn)
		}
	}
}

// --- dashboard template escapes the new activity/dispatch/snapshot/browser fields before interpolating ---

// extractJSFunc returns the body of the first JS function named name in src, the same
// start/end-delimiter approach TestRenderDispatchesTemplateEscapesStatusFields uses.
func extractJSFunc(t *testing.T, src, name string) string {
	t.Helper()
	start := strings.Index(src, "function "+name)
	if start < 0 {
		t.Fatalf("function %s not found in the dashboard template", name)
	}
	end := strings.Index(src[start:], "\n}\n")
	if end < 0 {
		t.Fatalf("could not find the end of %s", name)
	}
	return src[start : start+end]
}

func templateSource(t *testing.T) string {
	t.Helper()
	src, err := assetsFS.ReadFile("assets/index.html.tmpl")
	if err != nil {
		t.Fatalf("read index.html.tmpl: %v", err)
	}
	return string(src)
}

func TestRenderActivityEscapesToolAndText(t *testing.T) {
	fn := extractJSFunc(t, templateSource(t), "renderActivity")
	for _, field := range []string{"text"} {
		if !strings.Contains(fn, "escapeHTML("+field) {
			t.Errorf("renderActivity interpolates %s without escapeHTML(...): regression risk for an agent's own text/tool output\n%s", field, fn)
		}
	}
}

func TestRenderLegEscapesVendorFields(t *testing.T) {
	fn := extractJSFunc(t, templateSource(t), "renderDispatch")
	for _, field := range []string{"l.fallback", "l.agent", "l.model", "l.duration"} {
		if !strings.Contains(fn, "escapeHTML("+field) {
			t.Errorf("renderDispatch interpolates %s without escapeHTML(...): regression risk for vendor-reported agent/model/fallback text\n%s", field, fn)
		}
	}
}

func TestRenderSubagentsEscapesDataAttributes(t *testing.T) {
	fn := extractJSFunc(t, templateSource(t), "renderSubagents")
	for _, field := range []string{"sessionId", "s.id"} {
		if !strings.Contains(fn, "escapeHTML("+field) {
			t.Errorf("renderSubagents interpolates %s without escapeHTML(...): regression risk in the data-session/data-agent attributes\n%s", field, fn)
		}
	}
}

func TestRenderSnapshotEscapesLabelAndFields(t *testing.T) {
	fn := extractJSFunc(t, templateSource(t), "renderSnapshot")
	for _, field := range []string{"snap.label || snap.id", "snap.workflow", "snap.model || ''", "snap.result", "lines"} {
		if !strings.Contains(fn, "escapeHTML("+field) {
			t.Errorf("renderSnapshot interpolates %s without escapeHTML(...): regression risk for agent-controlled label/result/event text\n%s", field, fn)
		}
	}
}

// --- dashboard template escapes workflow-run fields (name/phase; status goes through chip()) ---

func TestRenderWorkflowsEscapesNameAndPhase(t *testing.T) {
	fn := extractJSFunc(t, templateSource(t), "renderWorkflows")
	for _, field := range []string{"run.name || run.id", "phase.name"} {
		if !strings.Contains(fn, "escapeHTML("+field) {
			t.Errorf("renderWorkflows interpolates %s without escapeHTML(...): regression risk for a workflow run's own name/phase text\n%s", field, fn)
		}
	}
	// run.status/is rendered only through chip(...), which escapes internally — not a bare escapeHTML(...) call.
	if !strings.Contains(fn, "chip(run.status)") {
		t.Errorf("renderWorkflows must route run.status through chip(...) (which itself escapes)\n%s", fn)
	}
}

func TestRenderBrowsersEscapesReportFields(t *testing.T) {
	fn := extractJSFunc(t, templateSource(t), "renderBrowsers")
	for _, field := range []string{"b.displayName || b.name", "b.host || ''"} {
		if !strings.Contains(fn, "escapeHTML("+field) {
			t.Errorf("renderBrowsers interpolates %s without escapeHTML(...): regression risk for browser-report text\n%s", field, fn)
		}
	}
}

type simulatedSSHErr string

func (e simulatedSSHErr) Error() string { return string(e) }

var errSimulatedSSH = simulatedSSHErr("ssh: connection refused")

// Regression: a fetch that began before an SSE render must not overwrite it; the guard lives in the page script.
func TestIndexTreeFetchGuardedByEpoch(t *testing.T) {
	src := templateSource(t)
	load := extractJSFunc(t, src, "loadTree")
	for _, want := range []string{"const started = treeEpoch", "if (treeEpoch !== started) return;"} {
		if !strings.Contains(load, want) {
			t.Fatalf("loadTree lacks %q", want)
		}
	}
	watch := extractJSFunc(t, src, "watchTree")
	if i, j := strings.Index(watch, "treeEpoch++"), strings.Index(watch, "paintTree("); i < 0 || j < 0 || i > j {
		t.Fatal("watchTree must bump treeEpoch before rendering an SSE snapshot")
	}
	if !strings.Contains(watch, "es.onerror") || !strings.Contains(src, "if (!treeLive) loadTree();") {
		t.Fatal("polling fallback missing")
	}
}

// Runs the page's real loadTree/watchTree in node: a fetch that resolves after an SSE snapshot must not overwrite it.
func TestIndexStaleFetchLosesToSSEInNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH")
	}
	src := templateSource(t)
	script := `let treeEpoch = 0; let treeLive = false;
const el = {innerHTML: ''}; const document = {getElementById: () => el};
function renderTree(t) { return t.v; }
let resolveFetch; function fetch() { return new Promise(r => { resolveFetch = r; }); }
let listener; const window = {EventSource: true};
function EventSource() { this.addEventListener = (n, f) => { if (n === 'tree') listener = f; }; this.close = () => {}; }
` + extractJSFunc(t, src, "loadTree") + "\n}\n" + extractJSFunc(t, src, "watchTree") + "\n}" + `
loadTree(); watchTree();
listener({data: JSON.stringify({v: 'sse'})});
resolveFetch({json: () => ({v: 'stale'})});
setTimeout(() => { process.stdout.write(el.innerHTML); }, 10);
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	if string(out) != "sse" {
		t.Fatalf("tree shows %q after a stale fetch resolved, want the SSE snapshot \"sse\"", out)
	}
}

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

// countingBrowserEnv returns a browser.Env whose Run never touches a real binary (every probe
// reports "not found") and counts how many times it was invoked, so cache-hit vs. cache-miss
// behavior can be asserted without mdfind/xdg-settings/defaults ever running for real.
func countingBrowserEnv(t *testing.T) (browser.Env, *int) {
	t.Helper()
	n := 0
	var mu sync.Mutex
	return browser.Env{
		Home:       t.TempDir(),
		AppSupport: t.TempDir(),
		GOOS:       "linux", // deterministic probe set off this machine's actual installed browsers
		Run: func(name string, args ...string) (string, error) {
			mu.Lock()
			n++
			mu.Unlock()
			return "", errNotFound
		},
		Now: time.Now,
	}, &n
}

type notFoundErr string

func (e notFoundErr) Error() string { return string(e) }

const errNotFound = notFoundErr("not found")

// newBrowserTestServer is newTestServer plus an injected fake browser.Env and clock, mirroring
// newStatusTestServer's pattern for /api/dispatches.
func newBrowserTestServer(t *testing.T, env browser.Env, now func() time.Time) *Server {
	t.Helper()
	s := newTestServer(t, Options{}, nil)
	s.browserEnv = env
	s.browsers = newBrowserCache()
	s.now = now
	return s
}

// --- /api/browsers guard (host allowlist) ---

func TestHandleBrowsersGuardRejectsBadHost(t *testing.T) {
	env, _ := countingBrowserEnv(t)
	s := newBrowserTestServer(t, env, time.Now)
	req := newReq(t, "GET", "/api/browsers", "evil.example")
	w := do(s, req)
	if w.Code != http.StatusMisdirectedRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusMisdirectedRequest)
	}
}

func TestHandleBrowsersGuardRejectsCrossOrigin(t *testing.T) {
	env, _ := countingBrowserEnv(t)
	s := newBrowserTestServer(t, env, time.Now)
	req := newReq(t, "GET", "/api/browsers", "127.0.0.1:7777")
	req.Header.Set("Origin", "http://evil.example")
	w := do(s, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
}

// --- decodes to browser.Report ---

func TestHandleBrowsersDecodesReport(t *testing.T) {
	env, _ := countingBrowserEnv(t)
	s := newBrowserTestServer(t, env, time.Now)
	w := do(s, newReq(t, "GET", "/api/browsers", "127.0.0.1:7777"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var rep browser.Report
	if err := json.Unmarshal(w.Body.Bytes(), &rep); err != nil {
		t.Fatalf("decode /api/browsers body into browser.Report: %v; body=%s", err, w.Body.String())
	}
	if len(rep.Browsers) == 0 {
		t.Error("Report.Browsers is empty, want every known browser reported (even if not-installed)")
	}
}

// --- cached within the TTL: one browser.Detect probe pass across three requests ---

func TestHandleBrowsersServesWithinTTLFromCacheWithoutReDetecting(t *testing.T) {
	env, calls := countingBrowserEnv(t)
	fakeNow := time.Now()
	s := newBrowserTestServer(t, env, func() time.Time { return fakeNow })

	for i := range 3 {
		w := do(s, newReq(t, "GET", "/api/browsers", "127.0.0.1:7777"))
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i, w.Code)
		}
	}
	first := *calls
	if first == 0 {
		t.Fatal("expected at least one Env.Run call from the first request's detection pass")
	}
	// A second or third request within the cache TTL must not trigger any further Env.Run calls:
	// the call count after 3 requests must equal the count after request 1.
	w := do(s, newReq(t, "GET", "/api/browsers", "127.0.0.1:7777"))
	if w.Code != http.StatusOK {
		t.Fatalf("4th request: status = %d, want 200", w.Code)
	}
	if *calls != first {
		t.Errorf("4 requests within the cache TTL triggered more Env.Run calls: first-pass=%d, after=%d", first, *calls)
	}
}

// --- re-detects once the TTL has elapsed ---

func TestHandleBrowsersRedetectsAfterTTL(t *testing.T) {
	env, calls := countingBrowserEnv(t)
	fakeNow := time.Now()
	s := newBrowserTestServer(t, env, func() time.Time { return fakeNow })

	w1 := do(s, newReq(t, "GET", "/api/browsers", "127.0.0.1:7777"))
	if w1.Code != http.StatusOK {
		t.Fatalf("first request: status = %d, want 200", w1.Code)
	}
	first := *calls

	// Still within the TTL: no further probing.
	w2 := do(s, newReq(t, "GET", "/api/browsers", "127.0.0.1:7777"))
	if w2.Code != http.StatusOK {
		t.Fatalf("second request: status = %d, want 200", w2.Code)
	}
	if *calls != first {
		t.Fatalf("precondition failed: second request within TTL re-probed (calls %d -> %d)", first, *calls)
	}

	// Advance the fake clock past the cache TTL: the next request must trigger a fresh detection pass.
	fakeNow = fakeNow.Add(browserCacheTTL + time.Second)
	w3 := do(s, newReq(t, "GET", "/api/browsers", "127.0.0.1:7777"))
	if w3.Code != http.StatusOK {
		t.Fatalf("third request (post-expiry): status = %d, want 200", w3.Code)
	}
	if *calls <= first {
		t.Errorf("a request past the cache TTL should re-detect, got %d calls after expiry, want > %d", *calls, first)
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

// Spanning: the endpoint ticks with s.now(), then answers 403/409/400 and leaves the plan alone on each.
func TestPlanTickEndpoint(t *testing.T) {
	s, dir := plansServer(t)
	fixed := time.Date(2026, 10, 31, 12, 30, 0, 0, time.UTC)
	s.now = func() time.Time { return fixed }
	rel := "2026-10-01-old.md"
	full := filepath.Join(dir, "plans", rel)
	title := "1.2 Open owns: b.go"
	body := func(path, ttl string) map[string]any {
		return map[string]any{"path": path, "line": 6, "title": ttl, "done": true}
	}
	read := func() string { b, _ := os.ReadFile(full); return string(b) }
	orig := read()

	if w := do(s, tickReq(t, "", body(rel, title))); w.Code != http.StatusForbidden {
		t.Fatalf("no CSRF = %d, want 403", w.Code)
	}
	if w := do(s, tickReq(t, testCSRFToken, body(rel, "9.9 Nope"))); w.Code != http.StatusConflict {
		t.Fatalf("wrong title = %d %s, want 409", w.Code, w.Body)
	}
	for _, p := range []string{"../settings.md", "../../etc/passwd"} {
		if w := do(s, tickReq(t, testCSRFToken, body(p, title))); w.Code != http.StatusBadRequest {
			t.Fatalf("path %q = %d, want 400", p, w.Code)
		}
	}
	if read() != orig {
		t.Fatalf("rejected requests changed the plan:\n%s", read())
	}

	w := do(s, tickReq(t, testCSRFToken, body(rel, title)))
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"ok":true}` {
		t.Fatalf("tick = %d %s", w.Code, w.Body)
	}
	got := read()
	if !strings.Contains(got, "- [x] 1.2 Open") || !strings.Contains(got, "<!-- user: ticked 2026-10-31T12:30:00Z -->") ||
		!strings.Contains(got, "<!-- working: bob -->") {
		t.Fatalf("plan after tick:\n%s", got)
	}
}

// withEmptyDispatchDir points internal/agent's dispatch log at an empty temp dir for the duration of
// the test, so a /api/view request's vendor-dispatch lookup never touches this machine's real dispatch
// log under ~/.claude/state/harness/dispatch.
func withEmptyDispatchDir(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
}

// --- /api/view guard (host allowlist) ---

func TestHandleViewGuardRejectsBadHost(t *testing.T) {
	withEmptyDispatchDir(t)
	s := newTestServer(t, Options{ClaudeDir: agenttreeTestdata, Days: 36500}, nil)
	req := newReq(t, "GET", "/api/view?session=finished-session-aaaa1111&agent=fakewfagent002", "evil.example")
	w := do(s, req)
	if w.Code != http.StatusMisdirectedRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusMisdirectedRequest)
	}
}

func TestHandleViewGuardRejectsCrossOrigin(t *testing.T) {
	withEmptyDispatchDir(t)
	s := newTestServer(t, Options{ClaudeDir: agenttreeTestdata, Days: 36500}, nil)
	req := newReq(t, "GET", "/api/view?session=finished-session-aaaa1111&agent=fakewfagent002", "127.0.0.1:7777")
	req.Header.Set("Origin", "http://evil.example")
	w := do(s, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
}

// --- missing/unknown agent: 400/404, never 500 ---

func TestHandleViewMissingAgentIsBadRequest(t *testing.T) {
	withEmptyDispatchDir(t)
	s := newTestServer(t, Options{ClaudeDir: agenttreeTestdata, Days: 36500}, nil)
	w := do(s, newReq(t, "GET", "/api/view?session=finished-session-aaaa1111", "127.0.0.1:7777"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing ?agent: status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleViewUnknownAgentIsNotFoundNot500(t *testing.T) {
	withEmptyDispatchDir(t)
	s := newTestServer(t, Options{ClaudeDir: agenttreeTestdata, Days: 36500}, nil)
	w := do(s, newReq(t, "GET", "/api/view?agent=no-such-agent-at-all", "127.0.0.1:7777"))
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown agent: status = %d, want 404 (not 500); body=%s", w.Code, w.Body.String())
	}
}

// --- snapshot JSON decodes to tree.Snapshot ---

func TestHandleViewSnapshotDecodesAgenttreeSnapshot(t *testing.T) {
	withEmptyDispatchDir(t)
	s := newTestServer(t, Options{ClaudeDir: agenttreeTestdata, Days: 36500}, nil)
	req := newReq(t, "GET", "/api/view?session=finished-session-aaaa1111&agent=fakewfagent002", "127.0.0.1:7777")
	w := do(s, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var snap tree.Snapshot
	if err := json.Unmarshal(w.Body.Bytes(), &snap); err != nil {
		t.Fatalf("decode /api/view body into tree.Snapshot: %v; body=%s", err, w.Body.String())
	}
	if snap.ID != "fakewfagent002" {
		t.Errorf("snap.ID = %q, want fakewfagent002", snap.ID)
	}
	if snap.SessionID != "finished-session-aaaa1111" {
		t.Errorf("snap.SessionID = %q, want finished-session-aaaa1111", snap.SessionID)
	}
}

// --- follow=1 streams at least one line for an agent that already has a terminal result ---

func TestHandleViewFollowStreamsResultLine(t *testing.T) {
	withEmptyDispatchDir(t)
	s := newTestServer(t, Options{ClaudeDir: agenttreeTestdata, Days: 36500}, nil)
	req := newReq(t, "GET", "/api/view?session=finished-session-aaaa1111&agent=fakewfagent002&follow=1", "127.0.0.1:7777")
	w := do(s, req)
	if w.Code != http.StatusOK {
		t.Fatalf("follow=1: status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("follow=1 Content-Type = %q, want text/plain", ct)
	}
	// fakewfagent002's journal already carries a result (see internal/agenttree's own
	// TestViewSnapshotFields), so Follow emits that result line on its very first poll and returns.
	if lines := strings.TrimSpace(w.Body.String()); lines == "" {
		t.Fatal("follow=1 streamed zero lines for an already-resulted agent")
	} else if !strings.Contains(lines, "result") {
		t.Errorf("follow=1 body = %q, want a line containing %q", lines, "result")
	}
}

// --- follow=1 stops when the request context is cancelled, for an agent that never finishes ---

// TestHandleViewFollowStopsOnContextCancel uses an agent with no new events and no terminal result
// (agent-fakeagentid002.meta.json: "finished":false): the headers arrive at once (handleView flushes
// before the poll loop), the body stays open, and cancelling the request must end the server-side
// Follow loop and the body read promptly.
func TestHandleViewFollowStopsOnContextCancel(t *testing.T) {
	withEmptyDispatchDir(t)
	s := newTestServer(t, Options{ClaudeDir: agenttreeTestdata, Days: 36500}, nil)
	// A real server (not httptest.ResponseRecorder) so cancelling mid-request is a genuine client
	// disconnect the server side can observe via r.Context().
	srv := httptest.NewServer(s.mux)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET",
		srv.URL+"/api/view?session=finished-session-aaaa1111&agent=fakeagentid002&follow=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "127.0.0.1:7777"

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do() must return as soon as the headers are flushed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	errc := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(resp.Body) // blocks until the stream ends
		errc <- err
	}()

	time.Sleep(200 * time.Millisecond) // let handleView reach tree.Follow's poll loop
	cancel()

	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("the body ended cleanly although the request was cancelled before any event")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("body read error = %v, want it to wrap context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling the request context did not unblock handleView's follow within 5s")
	}
}

// --- follow=1 must survive the server's own WriteTimeout: handleView clears the write deadline,
// so a late event (after the timeout would otherwise have fired) still reaches the client ---

func TestFollowSurvivesWriteTimeout(t *testing.T) {
	withEmptyDispatchDir(t)
	claudeDir := t.TempDir()
	if err := copyDir(t, agenttreeTestdata, claudeDir); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(claudeDir, "projects", "-Users-fake-demo-repo",
		"finished-session-aaaa1111", "subagents", "agent-fakeagentid002.jsonl")

	s := newTestServer(t, Options{ClaudeDir: claudeDir, Days: 36500}, nil)
	ts := httptest.NewUnstartedServer(s.mux)
	ts.Config.WriteTimeout = 300 * time.Millisecond
	ts.Start()
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET",
		ts.URL+"/api/view?session=finished-session-aaaa1111&agent=fakeagentid002&follow=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "127.0.0.1:7777"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do(): %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	go func() {
		time.Sleep(350 * time.Millisecond) // past ts.Config.WriteTimeout; Follow emits it at its 500ms tick
		f, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return
		}
		defer f.Close()
		fmt.Fprintln(f, `{"type":"assistant","message":{"content":[{"type":"text","text":"after write timeout"}]}}`)
	}()

	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if strings.Contains(sc.Text(), "after write timeout") {
			return
		}
	}
	t.Fatalf("stream ended without the post-timeout line; scanner err=%v", sc.Err())
}

// copyDir recursively copies src into dst (dst must already exist).
func copyDir(t *testing.T, src, dst string) error {
	t.Helper()
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, raw, 0o600)
	})
}
