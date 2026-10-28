package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// rawUp is an upstream that records raw requests (the shared newUpstream insists on a JSON body).
type rawUp struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []recorded
}

type recorded struct {
	method, uri, body string
	hdr               http.Header
}

func newRawUp(t *testing.T, h func(w http.ResponseWriter, r *http.Request, n int)) *rawUp {
	u := &rawUp{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.reqs = append(u.reqs, recorded{r.Method, r.RequestURI, string(b), r.Header.Clone()})
		n := len(u.reqs)
		u.mu.Unlock()
		h(w, r, n)
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *rawUp) got() []recorded {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]recorded(nil), u.reqs...)
}

func okRaw(w http.ResponseWriter, _ *http.Request, _ int) {
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, `{"ok":true}`)
}

// ptOverlay: "pt" serves openai, responses and gemini; "oa" serves openai only.
func ptOverlay(up string) string {
	return `{"default":"pt","providers":{"pt":{"kind":"openai","base_url":"` + up + `/v1","auth":{"type":"key","env":"PT_KEY"},` +
		`"routes":{"responses":"` + up + `/resp","gemini":"` + up + `/gem"},"models":{"opus":"m"}},` +
		`"oa":{"kind":"openai","base_url":"` + up + `/v1","auth":{"type":"key","env":"PT_KEY"},"models":{"opus":"m"}}}}`
}

func ptDo(t *testing.T, method, url, body string, hdr map[string]string) *http.Response {
	t.Helper()
	req := testRequest(t, method, url, strings.NewReader(body))
	req.Header.Set("X-Api-Key", secret)
	for k, v := range hdr {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
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
	// ClientHeader carries the secret so Authorization survives ServeHTTP's own strip: passthrough must drop it.
	resp := ptDo(t, http.MethodPost, srv.URL+"/p/pt/openai/chat/completions", `{}`, map[string]string{
		"X-Api-Key": "", ClientHeader: secret,
		"Authorization": "Bearer inbound-leak", "x-goog-api-key": "goog-leak"})
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	h := up.got()[0].hdr
	for _, k := range []string{"X-Api-Key", "X-Goog-Api-Key", ClientHeader} {
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
	ptDo(t, http.MethodPost, srv.URL+"/p/pt/gemini/models/g:generateContent", `{}`, map[string]string{"x-goog-api-key": "goog-leak"})
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
	overlay, _ := json.Marshal(map[string]any{"default": name, "providers": map[string]any{name: map[string]any{
		"kind": "openai", "base_url": up.URL + "/v1", "auth": map[string]any{"type":"key", "command": cmd, "ttl": 3600},
		"models": map[string]string{"opus": "m"}}}})
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
	big, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/p/pt/openai/x", io.LimitReader(zeroReader{}, maxBody+1))
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
			cmd := fmt.Sprintf(`n=$(cat %q 2>/dev/null || echo 0); n=$((n+1)); echo $n > %q; echo key$n`, counter, counter)
			overlay, _ := json.Marshal(map[string]any{"default": name, "providers": map[string]any{name: map[string]any{
				"kind": "openai", "base_url": up.URL + "/v1", "auth": map[string]any{"type": c.auth, "command": cmd, "ttl": 3600},
				"models": map[string]string{"opus": "m"}}}})
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
