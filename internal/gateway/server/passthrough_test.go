package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	_ "github.com/edotau/claude-code/internal/codex" // register the rows whose suffixes are checked
	_ "github.com/edotau/claude-code/internal/copilot"
	"github.com/edotau/claude-code/internal/gateway"
	"github.com/edotau/claude-code/internal/gateway/translate"
	"github.com/edotau/claude-code/internal/models"
	_ "github.com/edotau/claude-code/internal/opencode"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// ptOverlay: "pt" serves openai, responses and gemini; "oa" serves openai only.
func ptOverlay(up string) string {
	return `{"default":"pt","providers":{"openai":{"pt":{"protocol":"openai","base_url":"` + up + `/v1","auth":{"type":"key","env":"PT_KEY"},` +
		`"routes":{"responses":"` + up + `/resp","gemini":"` + up + `/gem"},"models":{"opus":"m"}},` +
		`"oa":{"protocol":"openai","base_url":"` + up + `/v1","auth":{"type":"key","env":"PT_KEY"},"models":{"opus":"m"}}}}}`
}

func TestPassthroughForwardsVerbatim(t *testing.T) {
	cases := []struct{ dialect, route string }{
		{"openai", "/v1"}, {"responses", "/resp"}, {"gemini", "/gem"},
	}
	for _, c := range cases {
		t.Run(c.dialect, func(t *testing.T) {
			up := newRawUp(t, okRaw)
			t.Setenv("PT_KEY", "prov-cred")
			srv := setup(t, ptOverlay(up.URL))
			body := `{"raw": "not-reformatted",  "n":1}`
			resp := ptDo(t, http.MethodPost, srv.URL+"/p/pt/"+c.dialect+"/models/x:generate?alt=sse&k=a%20b", body, nil)
			b, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != 200 || string(b) != `{"ok":true}` {
				t.Fatalf("status %d body %q", resp.StatusCode, b)
			}
			g := up.got()
			if len(g) != 1 {
				t.Fatalf("upstream hits %d, want 1", len(g))
			}
			if want := c.route + "/models/x:generate?alt=sse&k=a%20b"; g[0].uri != want {
				t.Fatalf("upstream uri %q, want %q", g[0].uri, want)
			}
			if g[0].method != http.MethodPost || g[0].body != body {
				t.Fatalf("method %q body %q, want verbatim POST", g[0].method, g[0].body)
			}
		})
	}
}

// A transient passthrough 429 is relayed and cools the provider in this proxy; a model-denied 429 does not.
func TestPassthrough429CoolsOnlyOnTransientLimits(t *testing.T) {
	cases := []struct {
		name, body string
		cools      bool
	}{
		{"transient", `{"error":{"message":"slow down"}}`, true},
		{"model denied", `{"error":{"message":"Quota exceeded for metric: x, limit: 0, model: m"}}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			up := newRawUp(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
				w.Header().Set("Retry-After", "60")
				w.WriteHeader(http.StatusTooManyRequests)
				io.WriteString(w, c.body)
			})
			t.Setenv("PT_KEY", "prov-cred")
			vaulttest.Use(t)
			vaulttest.Put(t, vault.Overlay, "providers", ptOverlay(up.URL))
			r := newRouter(secret)
			r.log = io.Discard
			srv := httptest.NewServer(r)
			t.Cleanup(func() { srv.Close(); r.bg.Wait() }) // a late state write must not race TempDir removal
			resp := ptDo(t, http.MethodPost, srv.URL+"/p/pt/openai/chat/completions", `{"model":"m"}`, nil)
			if b, _ := io.ReadAll(resp.Body); resp.StatusCode != 429 || string(b) != c.body {
				t.Fatalf("relayed %d %q, want the upstream 429 untouched", resp.StatusCode, b)
			}
			r.mu.Lock()
			until, cooling := r.cool["pt"]
			r.mu.Unlock()
			if cooling != c.cools || cooling && !until.After(time.Now().Add(30*time.Second)) {
				t.Fatalf("cooling=%v until %v, want cooling=%v honoring Retry-After 60", cooling, until, c.cools)
			}
		})
	}
}

func TestPassthroughGET(t *testing.T) {
	up := newRawUp(t, okRaw)
	t.Setenv("PT_KEY", "prov-cred")
	srv := setup(t, ptOverlay(up.URL))
	resp := ptDo(t, http.MethodGet, srv.URL+"/p/pt/openai/models?limit=3", "", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	g := up.got()
	if len(g) != 1 || g[0].method != http.MethodGet || g[0].uri != "/v1/models?limit=3" || g[0].body != "" {
		t.Fatalf("upstream saw %+v, want GET /v1/models?limit=3 with no body", g)
	}
}

func TestPassthroughNotFound(t *testing.T) {
	up := newRawUp(t, okRaw)
	t.Setenv("PT_KEY", "prov-cred")
	srv := setup(t, ptOverlay(up.URL))
	for path, want := range map[string]int{
		"/p/pt/bogus/models":  404, // unknown dialect
		"/p/oa/gemini/models": 404, // provider lacks that route
		"/p/oa/responses/x":   404, // provider lacks that route
		"/p/nosuch/openai/x":  400, // unknown provider
	} {
		if resp := ptDo(t, http.MethodGet, srv.URL+path, "", nil); resp.StatusCode != want {
			t.Errorf("%s: got %d, want %d", path, resp.StatusCode, want)
		}
	}
	if n := len(up.got()); n != 0 {
		t.Fatalf("upstream reached %d times on rejected paths", n)
	}
}

func TestPassthroughStripsInboundAuth(t *testing.T) {
	up := newRawUp(t, okRaw)
	t.Setenv("PT_KEY", "prov-cred")
	srv := setup(t, ptOverlay(up.URL))
	// gateway.ClientHeader carries the secret so Authorization survives ServeHTTP's own strip: passthrough must drop it.
	resp := ptDo(t, http.MethodPost, srv.URL+"/p/pt/openai/chat/completions", `{}`, map[string]string{
		"X-Api-Key": "", gateway.ClientHeader: secret,
		"Authorization": "Bearer inbound-leak", "x-goog-api-key": "goog-leak"})
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	h := up.got()[0].hdr
	for _, k := range []string{"X-Api-Key", "X-Goog-Api-Key", gateway.ClientHeader} {
		if v := h.Get(k); v != "" {
			t.Errorf("header %s reached upstream: %q", k, v)
		}
	}
	if got := h.Values("Authorization"); len(got) != 1 || got[0] != "Bearer prov-cred" {
		t.Fatalf("upstream Authorization = %q, want only the provider credential", got)
	}
	for k, vs := range h {
		for _, v := range vs {
			if strings.Contains(v, "leak") || v == secret {
				t.Errorf("inbound value leaked in %s: %q", k, v)
			}
		}
	}
}

func TestPassthroughStripsGoogKeyWithPlainSecret(t *testing.T) {
	up := newRawUp(t, okRaw)
	t.Setenv("PT_KEY", "prov-cred")
	srv := setup(t, ptOverlay(up.URL))
	ptDo(t, http.MethodPost, srv.URL+"/p/pt/gemini/models/g:generateContent", `{}`,
		map[string]string{"x-goog-api-key": "goog-leak"})
	h := up.got()[0].hdr
	if h.Get("X-Goog-Api-Key") != "" || h.Get("X-Api-Key") == secret || h.Get("Authorization") != "Bearer prov-cred" {
		t.Fatalf("upstream headers %v", h)
	}
}

func TestPassthroughWrongSecret(t *testing.T) {
	up := newRawUp(t, okRaw)
	t.Setenv("PT_KEY", "prov-cred")
	srv := setup(t, ptOverlay(up.URL))
	for _, h := range []map[string]string{{"X-Api-Key": "wrong"}, {"X-Api-Key": ""}} {
		if resp := ptDo(t, http.MethodPost, srv.URL+"/p/pt/openai/chat/completions", `{}`, h); resp.StatusCode != 401 {
			t.Errorf("headers %v: got %d, want 401", h, resp.StatusCode)
		}
	}
	if n := len(up.got()); n != 0 {
		t.Fatalf("upstream reached %d times without a valid secret", n)
	}
}

var ptRefreshRun atomic.Uint64

func TestPassthroughRefreshReplay(t *testing.T) {
	name := fmt.Sprintf("ptr%d", ptRefreshRun.Add(1))
	counter := filepath.Join(t.TempDir(), "n")
	up := newRawUp(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		if r.Header.Get("Authorization") != "Bearer key2" {
			w.WriteHeader(401)
			return
		}
		okRaw(w, r, 0)
	})
	cmd := fmt.Sprintf(`n=$(cat %q 2>/dev/null || echo 0); n=$((n+1)); echo $n > %q; echo key$n`, counter, counter)
	overlay, _ := json.Marshal(
		map[string]any{"default": name, "providers": map[string]any{"openai": map[string]any{name: map[string]any{
			"protocol": "openai", "base_url": up.URL + "/v1", "auth": map[string]any{"type": "key", "command": cmd, "ttl": 3600},
			"models": map[string]string{"opus": "m"}}}}},
	)
	srv := setup(t, string(overlay))

	body := `{"keep":"me"}`
	resp := ptDo(t, http.MethodPost, srv.URL+"/p/"+name+"/openai/chat/completions", body, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d, want 200 after one replay", resp.StatusCode)
	}
	g := up.got()
	if len(g) != 2 {
		t.Fatalf("upstream hits %d, want exactly 2 (one replay)", len(g))
	}
	if g[1].body != body {
		t.Fatalf("replayed body %q, want %q", g[1].body, body)
	}
}

func TestPassthroughMethodSizeAndLegacyRoutes(t *testing.T) {
	up := newRawUp(t, okRaw)
	t.Setenv("PT_KEY", "prov-cred")
	srv := setup(t, ptOverlay(up.URL))
	if resp := ptDo(t, http.MethodPut, srv.URL+"/p/pt/openai/x", `{}`, nil); resp.StatusCode != 404 {
		t.Errorf("PUT: got %d, want 404", resp.StatusCode)
	}
	if resp := ptDo(t, http.MethodPost, srv.URL+"/p/pt/anthropic/foo", `{}`, nil); resp.StatusCode != 404 {
		t.Errorf("anthropic dialect is not passthrough: got %d, want 404", resp.StatusCode)
	}
	big, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/p/pt/openai/x",
		io.LimitReader(zeroReader{}, maxBody+1))
	if err != nil {
		t.Fatal(err)
	}
	big.Header.Set("X-Api-Key", secret)
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(big)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 413 {
		t.Errorf("oversize body: got %d, want 413", resp.StatusCode)
	}
	if n := len(up.got()); n != 0 {
		t.Fatalf("upstream reached %d times on rejected requests", n)
	}
}

func TestPassthroughStreamsSSE(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	up := newRawUp(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	t.Setenv("PT_KEY", "prov-cred")
	srv := setup(t, ptOverlay(up.URL))
	resp := ptDo(t, http.MethodPost, srv.URL+"/p/pt/responses/responses", `{"stream":true}`, nil)
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type %q", ct)
	}
	line := make(chan string, 1)
	go func() {
		l, _ := bufio.NewReader(resp.Body).ReadString('\n')
		line <- l
	}()
	select {
	case l := <-line:
		if l != "data: first\n" {
			t.Fatalf("first line %q", l)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first SSE event not flushed while upstream still open")
	}
}

func TestPassthroughReplayCredentialHeaders(t *testing.T) {
	for _, c := range []struct{ auth, header, want string }{
		{"bearer", "Authorization", "Bearer key2"},
		{"x-api-key", "X-Api-Key", "key2"},
	} {
		t.Run(c.auth, func(t *testing.T) {
			name := fmt.Sprintf("ptc%d", ptRefreshRun.Add(1))
			counter := filepath.Join(t.TempDir(), "n")
			up := newRawUp(t, func(w http.ResponseWriter, r *http.Request, _ int) {
				if r.Header.Get(c.header) != c.want {
					w.WriteHeader(401)
					return
				}
				okRaw(w, r, 0)
			})
			cmd := fmt.Sprintf(`n=$(cat %q 2>/dev/null || echo 0); n=$((n+1)); echo $n > %q; echo key$n`, counter,
				counter)
			overlay, _ := json.Marshal(
				map[string]any{
					"default": name,
					"providers": map[string]any{"openai": map[string]any{name: map[string]any{
						"protocol": "openai", "base_url": up.URL + "/v1",
						"auth":   map[string]any{"type": "key", "header": c.auth, "command": cmd, "ttl": 3600},
						"models": map[string]string{"opus": "m"}}}},
				},
			)
			srv := setup(t, string(overlay))
			resp := ptDo(t, http.MethodPost, srv.URL+"/p/"+name+"/openai/chat/completions", `{}`,
				map[string]string{"x-goog-api-key": "goog-leak"})
			g := up.got()
			if resp.StatusCode != 200 || len(g) != 2 {
				t.Fatalf("status %d hits %d, want 200 after exactly one replay", resp.StatusCode, len(g))
			}
			for i, r := range g {
				if v := r.hdr.Get("X-Goog-Api-Key"); v != "" {
					t.Errorf("request %d leaked X-Goog-Api-Key %q", i+1, v)
				}
				if r.hdr.Get("X-Api-Key") == secret || strings.Contains(r.hdr.Get("Authorization"), secret) {
					t.Errorf("request %d leaked the router secret", i+1)
				}
			}
		})
	}
}

// zeroReader yields endless zero bytes for the oversize-body case.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

// Finding 4: the upstream message is one quoted msg= token, last on the line, after any stream=broken.
func TestErrDiagQuotesMessageAndKeepsItLast(t *testing.T) {
	got := errDiag([]byte(`{"error_code":"E","message":"x dispatch=evil \"stream=broken\""}`))
	if want := ` err=E msg="x dispatch=evil \"stream=broken\""`; got != want {
		t.Errorf("errDiag = %q, want %q", got, want)
	}
	want := ` err=E stream=broken:eof msg="x dispatch=evil \"stream=broken\""`
	if line := addBroken(got, " stream=broken:eof"); line != want {
		t.Errorf("addBroken = %q, want %q", line, want)
	}
}

// A passthrough 429 logs sanitized diagnostics and relays the upstream body unchanged.
func TestPassthroughLogsDiagnostics(t *testing.T) {
	const upBody = `{"error_code":"REQUEST_LIMIT_EXCEEDED","message":"slow down"}`
	up := newRawUp(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("X-Request-Id", "abc-123\nforged=1")
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, upBody)
	})
	t.Setenv("PT_KEY", "prov-cred")
	logs := &lockedBuf{}
	srv := setupRouter(t, ptOverlay(up.URL), func(r *Router) { r.log = logs })

	resp := ptDo(t, http.MethodPost, srv.URL+"/p/pt/responses/responses",
		`{"model":"databricks-gpt-6-1-sol","input":"hi"}`, nil)
	if b := body(t, resp); resp.StatusCode != http.StatusTooManyRequests || b != upBody {
		t.Fatalf("status %d body %q, want the upstream 429 verbatim", resp.StatusCode, b)
	}
	want := []string{
		" pt databricks-gpt-6-1-sol 429 1 ",
		" req=abc-123forged1",
		" retry_after=30",
		" err=REQUEST_LIMIT_EXCEEDED",
	}
	deadline := time.Now().Add(2 * time.Second) // the log line is written after the reply
	for {
		line := logs.String()
		missing := ""
		for _, w := range want {
			if !strings.Contains(line, w) {
				missing = w
				break
			}
		}
		if missing == "" {
			if strings.Count(line, "\n") != 1 {
				t.Fatalf("log = %q, want exactly one line (a header must not inject a second)", line)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("log line %q lacks %q", line, missing)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestClampOutputTokensLowersOnlyOversizedCaps(t *testing.T) {
	limit, ok := models.Limits("claude-sonnet-5-5")
	if !ok || limit.Output <= 0 {
		t.Skip("catalog has no output limit for the model")
	}
	over := limit.Output + 1000
	in := fmt.Sprintf(`{"max_tokens":%d,"max_output_tokens":5}`, over)
	var got map[string]float64
	if err := json.Unmarshal(clampOutputTokens([]byte(in), "claude-sonnet-5-5"), &got); err != nil {
		t.Fatal(err)
	}
	if got["max_tokens"] != float64(limit.Output) || got["max_output_tokens"] != 5 {
		t.Fatalf("caps = %v, want max_tokens %d and max_output_tokens 5", got, limit.Output)
	}
}

func TestGeminiSignaturesSignsOnlyUnsignedCalls(t *testing.T) {
	top := map[string]json.RawMessage{"messages": json.RawMessage(
		`[{"tool_calls":[null,{"id":"a"},{"id":"b","thoughtSignature":"s"},{"id":"c","extra_content":{}}]},{"role":"user"}]`,
	)}
	if !geminiSignatures(top, false) {
		t.Fatal("geminiSignatures = false, want true")
	}
	got := string(top["messages"])
	if strings.Count(got, `"thoughtSignature"`) != 2 || !strings.Contains(got, `"id":"b","thoughtSignature":"s"`) ||
		!strings.Contains(got, "null") {
		t.Fatalf("messages = %s, want only call a newly signed", got)
	}
}

// TestCopyClientHeadersDropsHopByHop: Proxy-Connection and every header the Connection value names stay hop-local.
func TestCopyClientHeadersDropsHopByHop(t *testing.T) {
	in := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	in.Header.Set("Connection", "keep-alive, X-Hop-Secret")
	in.Header.Set("Proxy-Connection", "keep-alive")
	in.Header.Set("X-Hop-Secret", "s")
	in.Header.Set("Anthropic-Version", "2023-06-01")
	out := http.Header{}
	copyClientHeaders(in)(out)
	for _, k := range []string{"Connection", "Proxy-Connection", "X-Hop-Secret"} {
		if out.Get(k) != "" {
			t.Errorf("%s forwarded upstream: %q", k, out.Get(k))
		}
	}
	if out.Get("Anthropic-Version") != "2023-06-01" {
		t.Errorf("end-to-end header dropped: %v", out)
	}
}

// Every registered row's router suffix parses back to its provider and wire; an empty suffix is no passthrough.
func TestRegisteredRowRouterSuffixesRoundTrip(t *testing.T) {
	seen := 0
	for _, spec := range agent.Roster() {
		for wire, suffix := range spec.CLI.RouterSuffix {
			if checkRouterSuffix(t, spec.Name, wire, suffix) {
				seen++
			}
		}
		for _, d := range spec.CLI.Dialects {
			if _, ok := spec.CLI.RouterSuffix[d]; !ok {
				t.Errorf("%s: dialect %q has no router suffix", spec.Name, d)
			}
		}
	}
	if seen == 0 {
		t.Fatal("no router suffix checked")
	}
}

// checkRouterSuffix verifies one row/wire suffix; true when a non-empty suffix was checked.
func checkRouterSuffix(t *testing.T, name, wire, suffix string) bool {
	t.Helper()
	base := gateway.SessionBase("http://127.0.0.1:1", "prov") + suffix
	path := base[len("http://127.0.0.1:1"):] + "/v1/x"
	prov, dialect, rest, ok := parsePassthrough(path)
	if suffix == "" {
		if ok {
			t.Errorf("%s/%s: empty suffix parsed as passthrough %q", name, wire, path)
		}
		return false
	}
	if !ok || prov != "prov" || dialect != wire || rest != "v1/x" {
		t.Errorf("%s/%s: %q -> (%q, %q, %q, %v), want dialect %q", name, wire, path, prov, dialect, rest, ok, wire)
	}
	return true
}

// A condemned refresh on the passthrough path (GET and POST) answers 401 and records the mark.
func TestPassthroughCondemnedRefreshAnswers401AndMarks(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			g := useGate(t)
			up := newRawUp(t, okRaw)
			srv := setupRouter(t,
				fmt.Sprintf(`{"default":"up1","providers":{"databricks":{"up1":{"protocol":"openai","base_url":%q,`+
					`"auth":{"type":"oauth"},"models":{"opus":"m"}}}}}`, up.URL+"/v1"),
				nil)
			resp := ptDo(t, method, srv.URL+"/p/up1/openai/models", "{}", nil)
			b := body(t, resp)
			if resp.StatusCode != http.StatusUnauthorized ||
				!strings.Contains(b, "authentication failed; run `claude-code databricks login --profile up1`") {
				t.Fatalf("status %d body %s, want 401 authentication failed with login hint", resp.StatusCode, b)
			}
			if len(up.got()) != 0 {
				t.Fatal("upstream reached without a credential")
			}
			if _, ok := g.marked("up1"); !ok {
				t.Fatal("passthrough path did not record the mark")
			}
		})
	}
}

// Regression: every passthrough dialect refuses a dot-segment tail before building the upstream URL.
func TestPassthroughRejectsDotSegments(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	up := newRawUp(t, okRaw)
	t.Setenv("PT_KEY", "prov-cred")
	srv := setup(t, ptOverlay(up.URL))
	for _, rest := range []string{"openai/%2e%2e/admin", "gemini/v1beta/%2e%2e/x", "responses/a%252e%252e/b"} {
		if resp := ptDo(t, http.MethodPost, srv.URL+"/p/pt/"+rest, `{}`, nil); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d", rest, resp.StatusCode)
		}
	}
	if g := up.got(); len(g) != 0 {
		t.Fatalf("upstream reached: %v", g[0].uri)
	}
}

const geminiChat = `{"model":"databricks-gemini-3-8-flash","stream":true,"stream_options":{"include_usage":true},"messages":[` +
	`{"role":"user","content":"hi"},` +
	`{"role":"assistant","tool_calls":[` +
	`{"id":"a","type":"function","function":{"name":"f","arguments":"{}"}},` +
	`{"id":"b","type":"function","thoughtSignature":"real-sig","function":{"name":"f","arguments":"{}"}},` +
	`{"id":"c","type":"function","extra_content":{"google":{"thought_signature":"x"}},"function":{"name":"f","arguments":"{}"}}]},` +
	`{"role":"tool","tool_call_id":"a","content":"ok"}]}`

func sendChat(t *testing.T, body string, h func(http.ResponseWriter, *http.Request, int)) (*rawUp, *http.Response) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	up := newRawUp(t, h)
	t.Setenv("PT_KEY", "prov-cred")
	srv := setup(t, ptOverlay(up.URL))
	return up, ptDo(t, http.MethodPost, srv.URL+"/p/pt/openai/chat/completions", body, nil)
}

// Passthrough to a Gemini model drops stream_options and signs unsigned assistant tool_calls only.
func TestPassthroughGeminiChatSanitized(t *testing.T) {
	up, resp := sendChat(t, geminiChat, okRaw)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	g := up.got()
	if len(g) != 1 {
		t.Fatalf("upstream hits %d", len(g))
	}
	var sent struct {
		StreamOptions *json.RawMessage `json:"stream_options"`
		Messages      []struct {
			ToolCalls []map[string]json.RawMessage `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(g[0].body), &sent); err != nil {
		t.Fatal(err)
	}
	if sent.StreamOptions != nil || strings.Contains(g[0].body, "stream_options") {
		t.Errorf("stream_options reached upstream: %s", g[0].body)
	}
	calls := sent.Messages[1].ToolCalls
	if string(calls[0]["thoughtSignature"]) != `"skip_thought_signature_validator"` {
		t.Errorf("unsigned call = %v, want skip signature", calls[0])
	}
	if string(calls[1]["thoughtSignature"]) != `"real-sig"` {
		t.Errorf("signed call changed: %v", calls[1])
	}
	if _, has := calls[2]["thoughtSignature"]; has || calls[2]["extra_content"] == nil {
		t.Errorf("extra_content call touched: %v", calls[2])
	}
}

// A non-Gemini chat body is forwarded byte-identical.
func TestPassthroughNonGeminiChatUntouched(t *testing.T) {
	body := strings.Replace(geminiChat, "databricks-gemini-3-8-flash", "databricks-llama", 1)
	up, resp := sendChat(t, body, okRaw)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if g := up.got(); len(g) != 1 || g[0].body != body {
		t.Fatalf("upstream body %+v, want unchanged", g)
	}

	// a 400 naming function tools with reasoning_effort is retried once with it set to none; other 400s are not
	const msg = `{"error":{"message":"Function tools with reasoning_effort are not supported for gpt-6.1-terra in /v1/chat/completions."}}`
	reqBody := `{"model":"databricks-gpt-6-1-terra","reasoning_effort":"high","tools":[{"type":"function","function":{"name":"f"}}]}`
	for _, tc := range []struct {
		name, errBody string
		status, hits  int
	}{
		{"capability error", msg, 200, 2},
		{"other 400", `{"error":{"message":"bad"}}`, 400, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rup, rresp := sendChat(t, reqBody, func(w http.ResponseWriter, r *http.Request, n int) {
				b, _ := io.ReadAll(r.Body)
				if n == 1 || bytes.Contains(b, []byte(`"reasoning_effort":"high"`)) {
					w.WriteHeader(400)
					io.WriteString(w, tc.errBody)
					return
				}
				okRaw(w, r, n)
			})
			g := rup.got()
			if rresp.StatusCode != tc.status || len(g) != tc.hits {
				t.Fatalf("status %d hits %d, want %d/%d", rresp.StatusCode, len(g), tc.status, tc.hits)
			}
			if tc.hits == 2 && (!strings.Contains(g[1].body, `"reasoning_effort":"none"`) || !strings.Contains(g[1].body, `"tools"`)) {
				t.Errorf("retry body %s, want tools kept and reasoning_effort none", g[1].body)
			}
		})
	}
}

// A null tool_calls entry passes through instead of panicking the handler.
func TestFixGeminiChatNullToolCall(t *testing.T) {
	in := `{"model":"databricks-gemini-3-8-flash","messages":[{"role":"assistant","tool_calls":[null,{"id":"a","type":"function"}]}]}`
	out := string(fixGeminiChat([]byte(in)))
	if !strings.Contains(out, "null") || !strings.Contains(out, translate.SkipThoughtSignature) {
		t.Fatalf("null entry or signature lost: %s", out)
	}
}

// An Anthropic-style tool_choice object becomes the chat string Databricks Gemini accepts; other forms stay.
func TestFixGeminiChatToolChoice(t *testing.T) {
	for in, want := range map[string]string{
		`{"type":"auto"}`: `"auto"`, `{"type":"any"}`: `"required"`, `"auto"`: `"auto"`, `"validated"`: `"auto"`,
		`{"type":"function","function":{"name":"p"}}`: `{"type":"function","function":{"name":"p"}}`,
	} {
		out := string(fixGeminiChat([]byte(`{"model":"databricks-gemini-3-8-flash","tool_choice":` + in + `}`)))
		if !strings.Contains(out, `"tool_choice":`+want) {
			t.Errorf("tool_choice %s -> %s, want %s", in, out, want)
		}
	}
}

// A Gemini chat reply whose content is an array of parts reaches the client as one string; other models' replies stay.
func TestPassthroughGeminiChatReplyFlattened(t *testing.T) {
	parts := func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w,
			`{"choices":[{"message":{"role":"assistant","content":[{"type":"text","text":"O","thoughtSignature":"s"},{"type":"text","text":"K"}]}}]}`)
	}
	_, resp := sendChat(t, geminiChat, parts)
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), `"content":"OK"`) {
		t.Errorf("gemini reply = %s, want content OK string", b)
	}
	_, resp = sendChat(t, strings.Replace(geminiChat, "databricks-gemini-3-8-flash", "databricks-llama", 1), parts)
	b, _ = io.ReadAll(resp.Body)
	if !strings.Contains(string(b), `"thoughtSignature"`) {
		t.Errorf("non-gemini reply rewritten: %s", b)
	}
}

// Tool schemas sent to Gemini lose the keys Google rejects; a clean schema and other models' schemas are untouched.
func TestPassthroughGeminiChatToolSchemaCleaned(t *testing.T) {
	body := `{"model":"databricks-gemini-3-8-flash","tools":[{"type":"function","function":{"name":"ls","parameters":` +
		`{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","properties":{"n":{"type":"integer","exclusiveMinimum":0},` +
		`"k":{"const":"a"}},"additionalProperties":false}}}]}`
	up, _ := sendChat(t, body, okRaw)
	got := up.got()[0].body
	for _, bad := range []string{"$schema", "exclusiveMinimum", "const"} {
		if strings.Contains(got, bad) {
			t.Errorf("%s reached gemini: %s", bad, got)
		}
	}
	if !strings.Contains(got, `"enum":["a"]`) || !strings.Contains(got, `"additionalProperties":false`) {
		t.Errorf("valid keys lost: %s", got)
	}
	other := strings.Replace(body, "databricks-gemini-3-8-flash", "databricks-llama", 1)
	if up, _ := sendChat(t, other, okRaw); up.got()[0].body != other {
		t.Errorf("non-gemini tools rewritten: %s", up.got()[0].body)
	}
}

// Passthrough chat usage (plain JSON and the SSE closing chunk) lands in the ledger.
func TestPassthroughMetersChatUsage(t *testing.T) {
	for name, tc := range map[string]struct{ ct, out string }{
		"json": {"application/json", `{"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":4,"total_tokens":15}}`},
		"sse": {"text/event-stream", "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"completion_tokens\":4,\"prompt_tokens\":11,\"total_tokens\":15}}\n\ndata: [DONE]\n\n"},
	} {
		t.Run(name, func(t *testing.T) {
			_, resp := sendChat(t, `{"model":"m"}`, func(w http.ResponseWriter, _ *http.Request, _ int) {
				w.Header().Set("Content-Type", tc.ct)
				io.WriteString(w, tc.out)
			})
			b, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != 200 || string(b) != tc.out {
				t.Fatalf("status %d body %q, want relayed unchanged", resp.StatusCode, b)
			}
			got := ledgerSettled(t, 1)
			if len(got) != 1 || got[0].Profile != "pt" || got[0].Model != "m" || got[0].Input != 11 ||
				got[0].Output != 4 {
				t.Fatalf("ledger = %+v, want pt/m 11 in 4 out", got)
			}
		})
	}
}

// A non-200 upstream answer records nothing.
func TestPassthroughNon200NotMetered(t *testing.T) {
	_, resp := sendChat(t, `{"model":"m"}`, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"usage":{"prompt_tokens":9,"completion_tokens":9}}`)
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", resp.StatusCode)
	}
	if got := ledgerSettled(t, 0); len(got) != 0 {
		t.Fatalf("ledger = %+v, want empty", got)
	}
}

func TestHeadTailBufferBoundsMemoryKeepsBothEnds(t *testing.T) {
	const start = `{"type":"message_start","message":{"usage":{"input_tokens":7,"output_tokens":1}}}`
	const end = `{"type":"message_delta","usage":{"output_tokens":9}}`
	var b headTailBuffer
	b.Write([]byte(start))
	for range 40 {
		b.Write(bytes.Repeat([]byte("x"), 64<<10))
	}
	b.Write([]byte(end))
	if got := len(b.Bytes()); got > 2*tailCap {
		t.Errorf("retained %d bytes, want <= %d", got, 2*tailCap)
	}
	if u := scrapeAnthropicUsage(b.Bytes()); u.input != 7 || u.output != 9 {
		t.Errorf("usage = %+v, want input 7 output 9 (max, cumulative) from head and tail", u)
	}
}

// agy's call reaches /serving-endpoints/gemini with the wire model, alt=sse only, fresh token, thoughts-off fixed.
func TestDatabricksGeminiRoute(t *testing.T) {
	up, base := dbxGeminiSetup(t)
	body := `{"contents":[],"generationConfig":{"thinkingConfig":{"includeThoughts":true,"thinkingBudget":0}}}`
	resp := ptDo(t, http.MethodPost, base+"v1beta/models/gemini-3.8-flash:streamGenerateContent?alt=sse&key=x", body,
		nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	g := up.got()
	if len(g) != 1 {
		t.Fatalf("upstream hits %d", len(g))
	}
	if want := "/serving-endpoints/gemini/v1beta/models/databricks-gemini-3-8-flash:streamGenerateContent?alt=sse"; g[0].uri != want {
		t.Errorf("uri = %s, want %s", g[0].uri, want)
	}
	if strings.Contains(g[0].body, "includeThoughts") || !strings.Contains(g[0].body, `"thinkingBudget":0`) {
		t.Errorf("body = %s", g[0].body)
	}
	if a := g[0].hdr.Get("Authorization"); a != "Bearer dbx-token" {
		t.Errorf("authorization = %q", a)
	}
}

// Regression: an encoded or literal climb, or any call but (stream)GenerateContent, never reaches the workspace.
func TestDatabricksGeminiRouteRejectsTraversal(t *testing.T) {
	up, base := dbxGeminiSetup(t)
	for _, rest := range []string{
		"v1beta/models/%2e%2e/%2e%2e/api/2.0/secrets:generateContent", "v1beta/../../api/2.0/clusters/list",
		"v1beta/models/m%252e:generateContent", "v1beta//models/m:generateContent", "v1beta/models/m:countTokens",
		"v1beta/models/m%5c:generateContent",
	} {
		resp := ptDo(t, http.MethodPost, base+rest, `{}`, nil)
		if resp.StatusCode < 400 {
			t.Errorf("%s: status %d", rest, resp.StatusCode)
		}
	}
	if g := up.got(); len(g) != 0 {
		t.Fatalf("upstream reached: %v", g[0].uri)
	}
}

// A passthrough chat stream cut mid-body still meters the usage it relayed before breaking the client stream.
func TestPassthroughStreamCutMidBodyIsMetered(t *testing.T) {
	_, resp := sendChat(t, `{"model":"m","stream":true}`, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"completion_tokens\":4,\"prompt_tokens\":11,\"total_tokens\":15}}\n\n")
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	})
	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Error("client read the cut stream to a clean EOF, want it broken")
	}
	if got := ledgerSettled(t, 1); len(got) != 1 || got[0].Input != 11 || got[0].Output != 4 {
		t.Errorf("ledger = %+v, want one entry with 11 in, 4 out", got)
	}
}
