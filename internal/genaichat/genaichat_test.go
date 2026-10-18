package genaichat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// sse writes one Server-Sent-Events "data:" frame; GenerateContentStream parses alt=sse responses this way.
func sse(w http.ResponseWriter, chunk string) {
	w.Write([]byte("data: " + chunk + "\n\n"))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// TestStream drives genaichat.Stream against a fake Gemini API (GOOGLE_GEMINI_BASE_URL override, verified
// against google.golang.org/genai@v1.75.0 base_url.go) so the real SDK streaming/usage decode path runs.
func TestStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "streamGenerateContent") {
			t.Errorf("unexpected request path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		sse(w, `{"candidates":[{"content":{"parts":[{"text":"pon"}],"role":"model"}}]}`)
		sse(w, `{"candidates":[{"content":{"parts":[{"text":"g"}],"role":"model"},"finishReason":"STOP"}],`+
			`"usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":2,"thoughtsTokenCount":3,"cachedContentTokenCount":1}}`)
	}))
	defer srv.Close()
	t.Setenv("GOOGLE_GEMINI_BASE_URL", srv.URL+"/")

	var out strings.Builder
	usage, text, err := Stream(context.Background(), "test-key", "gemini-3.8-flash-low", "ping", "medium", &out)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if text != "pong" || out.String() != "pong" {
		t.Errorf("text = %q, streamed %q, want %q", text, out.String(), "pong")
	}
	want := Usage{Input: 12, Output: 2, Thinking: 3, CacheRead: 1}
	if usage != want {
		t.Errorf("usage = %+v, want %+v", usage, want)
	}
}

// TestStream_HTTPError proves a non-2xx response surfaces as an error rather than a silent empty answer.
func TestStream_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":{"code":403,"message":"API key not valid","status":"PERMISSION_DENIED"}}`))
	}))
	defer srv.Close()
	t.Setenv("GOOGLE_GEMINI_BASE_URL", srv.URL+"/")

	_, _, err := Stream(context.Background(), "bad-key", "gemini-3.8-flash-low", "ping", "low", &strings.Builder{})
	if err == nil {
		t.Fatal("Stream: want error on HTTP 403, got nil")
	}
}
