package server

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/proxy"
)

// filesUp stands in for api.anthropic.com: it counts /v1/files uploads and records /v1/messages bodies.
type filesUp struct {
	*httptest.Server
	mu      sync.Mutex
	uploads int
	hdr     http.Header
	bodies  []string
	status  int // /v1/files status; 0 = 200
	msgCode int // /v1/messages status; 0 = 200
}

func newFilesUp(t *testing.T) *filesUp {
	t.Helper()
	u := &filesUp{}
	u.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		defer u.mu.Unlock()
		if r.URL.Path == "/v1/files" {
			u.uploads++
			u.hdr = r.Header.Clone()
			if u.status != 0 {
				w.WriteHeader(u.status)
				return
			}
			io.WriteString(w, `{"id":"file_abc"}`)
			return
		}
		u.bodies = append(u.bodies, string(b))
		if u.msgCode != 0 {
			w.WriteHeader(u.msgCode)
			return
		}
		ok(w, r, 0)
	}))
	t.Cleanup(u.Close)
	pointAnthropicHostAt(t, u.URL)
	return u
}

func (u *filesUp) lastBody() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.bodies[len(u.bodies)-1]
}

func bigB64(n int) string { return base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", n))) }

func imgBlock(data string) string {
	return `{"type":"image","cache_control":{"type":"ephemeral"},"source":{"type":"base64","media_type":"image/png","data":"` + data + `"}}`
}

// filesBody has the same large image at top level and inside a tool_result.
func filesBody(data string) string {
	return `{"model":"m","messages":[{"role":"user","content":[` + imgBlock(data) + `]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[` + imgBlock(data) + `]}]}]}`
}

var clientLogin = map[string]string{"X-Api-Key": "", proxy.ClientHeader: secret, "Authorization": "Bearer tok", "Anthropic-Beta": "client-beta"}

func filesRouter(t *testing.T, u *filesUp, extra string) *httptest.Server {
	t.Helper()
	return setupRouter(t, `{"default":"sub",`+extra+`"providers":{"anthropic":{"sub":{"protocol":"anthropic","base_url":"`+u.URL+
		`","auth":{"type":"cli"},"models":{"opus":"m"}}}}}`, func(r *Router) { r.client = u.Client() })
}

func TestFilesRewriteUploadsOnceThenCaches(t *testing.T) {
	u := newFilesUp(t)
	srv := filesRouter(t, u, "")
	data := bigB64(300 << 10)
	for i := 1; i <= 2; i++ {
		if resp := post(t, srv.URL+"/p/sub/v1/messages", filesBody(data), clientLogin); resp.StatusCode != 200 {
			t.Fatalf("request %d: status %d", i, resp.StatusCode)
		}
		got := u.lastBody()
		if strings.Contains(got, data) || strings.Count(got, `"file_id":"file_abc"`) != 2 || !strings.Contains(got, `"cache_control"`) {
			t.Fatalf("request %d: upstream body not rewritten (len %d)", i, len(got))
		}
	}
	if u.uploads != 1 {
		t.Fatalf("uploads = %d, want 1 (dedupe within a request, cache across requests)", u.uploads)
	}
	if v, b := u.hdr.Get("Anthropic-Version"), u.hdr.Get("Anthropic-Beta"); v != filesVersion || b != "" {
		t.Errorf("upload headers version=%q beta=%q, want %q and none", v, b, filesVersion)
	}
}

func TestFilesUploadFailureKeepsBase64(t *testing.T) {
	u := newFilesUp(t)
	u.status = 500
	srv := filesRouter(t, u, "")
	data := bigB64(300 << 10)
	if resp := post(t, srv.URL+"/p/sub/v1/messages", filesBody(data), clientLogin); resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if got := u.lastBody(); strings.Count(got, data) != 2 || strings.Contains(got, `"file"`) {
		t.Fatalf("failed upload must leave base64 inline")
	}
	if u.uploads != 1 {
		t.Errorf("uploads = %d, want 1 (stop after the first failure)", u.uploads)
	}
}

func TestFilesSmallAndCountTokensUntouched(t *testing.T) {
	u := newFilesUp(t)
	srv := filesRouter(t, u, "")
	small := bigB64(1 << 10)
	post(t, srv.URL+"/p/sub/v1/messages", filesBody(small), clientLogin)
	big := bigB64(300 << 10)
	post(t, srv.URL+"/p/sub/v1/messages/count_tokens", filesBody(big), clientLogin)
	if u.uploads != 0 || strings.Contains(u.lastBody(), `"file_id"`) {
		t.Fatalf("uploads = %d; small block and count_tokens must stay inline", u.uploads)
	}
}

func TestFilesFallbackOffOriginSeesBase64(t *testing.T) {
	u := newFilesUp(t)
	u.msgCode = 529
	gem := newUpstream(t, ok)
	t.Setenv("GEM_KEY", "k")
	srv := setupRouter(t, `{"default":"sub","fallback":["gem"],"providers":{"anthropic":{
		"sub":{"protocol":"anthropic","base_url":"`+u.URL+`","auth":{"type":"cli"},"models":{"opus":"m"}},
		"gem":`+anthropicProvider(gem.URL, "GEM_KEY", `{"opus":"g"}`)+`}}}`, func(r *Router) { r.client = u.Client() })
	data := bigB64(300 << 10)
	if resp := post(t, srv.URL+"/p/sub/v1/messages", filesBody(data), clientLogin); resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if !strings.Contains(u.lastBody(), `"file_id"`) {
		t.Fatalf("anthropic origin should have been rewritten")
	}
	if gem.hits.Load() != 1 || strings.Count(rawBody(gem), data) != 2 {
		t.Fatalf("fallback hits=%d: off-origin target must get the original base64 and no upload", gem.hits.Load())
	}
}

func rawBody(u *upstream) string {
	b, _ := json.Marshal(*u.body.Load())
	return string(b)
}

func TestFilesCacheKeyedByMediaType(t *testing.T) {
	u := newFilesUp(t)
	srv := filesRouter(t, u, "")
	data := bigB64(300 << 10)
	for _, mt := range []string{"text/plain", "application/pdf"} {
		body := `{"model":"m","messages":[{"role":"user","content":[{"type":"document","source":{"type":"base64","media_type":"` + mt + `","data":"` + data + `"}}]}]}`
		post(t, srv.URL+"/p/sub/v1/messages", body, clientLogin)
	}
	if u.uploads != 2 {
		t.Fatalf("uploads = %d, want 2: identical bytes under another media type need their own file", u.uploads)
	}
}

func TestStoreFileBoundsCache(t *testing.T) {
	r := &Router{now: time.Now}
	for i := range fileCacheMax + 10 {
		r.storeFile(strconv.Itoa(i), "f")
	}
	if len(r.files) > fileCacheMax {
		t.Fatalf("files cache holds %d entries, cap %d", len(r.files), fileCacheMax)
	}
}

// TestFilesCacheScopedToUpstreamCredential: a new vault/env credential is another account, so its file id is new.
func TestFilesCacheScopedToUpstreamCredential(t *testing.T) {
	u := newFilesUp(t)
	srv := setupRouter(t, `{"default":"sub","providers":{"anthropic":{"sub":{"protocol":"anthropic","base_url":"`+u.URL+
		`","auth":{"type":"key","header":"x-api-key","env":"FILES_SCOPE_KEY"},"models":{"opus":"m"}}}}}`,
		func(r *Router) { r.client = u.Client() })
	data := bigB64(300 << 10)
	for _, key := range []string{"sk-account-a", "sk-account-b"} {
		t.Setenv("FILES_SCOPE_KEY", key)
		post(t, srv.URL+"/p/sub/v1/messages", filesBody(data), map[string]string{proxy.ClientHeader: secret})
	}
	if u.uploads != 2 {
		t.Fatalf("uploads = %d, want 2: account b must not reuse account a's file id", u.uploads)
	}
}

// TestFilesOAuthRefusalCachedPerCredential: live, /v1/files answers a subscription bearer 404; stop re-uploading it.
func TestFilesOAuthRefusalCachedPerCredential(t *testing.T) {
	u := newFilesUp(t)
	u.status = http.StatusNotFound
	srv := filesRouter(t, u, "")
	data := bigB64(300 << 10)
	for i, auth := range []string{"Bearer tok", "Bearer tok", "Bearer tok2"} {
		login := map[string]string{proxy.ClientHeader: secret, "Authorization": auth}
		if resp := post(t, srv.URL+"/p/sub/v1/messages", filesBody(data), login); resp.StatusCode != 200 {
			t.Fatalf("request %d: status %d", i, resp.StatusCode)
		}
		if strings.Count(u.lastBody(), data) != 2 {
			t.Fatalf("request %d: refused upload must leave base64 inline", i)
		}
	}
	if u.uploads != 2 {
		t.Fatalf("uploads = %d, want 2: a refused bearer is not retried, a new credential is", u.uploads)
	}
}
