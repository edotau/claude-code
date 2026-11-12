package server

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/gateway"
	"github.com/edotau/claude-code/internal/tokenledger"
)

func chatChunk(content, usage string) string {
	return fmt.Sprintf("data: {\"id\":\"c\",\"model\":\"gpt-x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":%q}}]%s}\n\n",
		content, usage)
}

// Translated (/v1/messages -> chat/completions) replies land in the ledger: JSON, SSE, SSE past the tail cap, cut SSE.
func TestTranslatedChatMetersUsage(t *testing.T) {
	const usage = `,"usage":{"prompt_tokens":11,"completion_tokens":4}`
	pad := strings.Repeat("x", 1<<10)
	big := strings.Repeat(chatChunk(pad, ""), 3*tailCap/len(pad)) + chatChunk("end", usage) + "data: [DONE]\n\n"
	for name, tc := range map[string]struct {
		stream  bool
		out     string
		cut     bool
		wantOut string
	}{
		"json":                {false, `{"id":"c","model":"gpt-x","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]` + usage + `}`, false, "hi"},
		"sse":                 {true, chatChunk("hi", usage) + "data: [DONE]\n\n", false, "hi"},
		"sse-over-tail-cap":   {true, big, false, "end"},
		"sse-cut-after-usage": {true, chatChunk("hi", usage), true, "hi"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			up := newRawUp(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
				if tc.stream {
					w.Header().Set("Content-Type", "text/event-stream")
				}
				io.WriteString(w, tc.out)
				if tc.cut {
					w.(http.Flusher).Flush()
					panic(http.ErrAbortHandler)
				}
			})
			t.Setenv("OA_KEY", "oa")
			srv := setup(t, `{"default":"oa","providers":{"openai":{"oa":{"protocol":"openai","base_url":"`+up.URL+
				`/v1","auth":{"type":"key","env":"OA_KEY"},"models":{"opus":"gpt-x"}}}}}`)
			req := fmt.Sprintf(`{"model":"gpt-x","max_tokens":10,"stream":%t,"messages":[{"role":"user","content":"hi"}]}`, tc.stream)
			resp := post(t, srv.URL+"/p/oa/v1/messages", req, map[string]string{
				"X-Claude-Code-Session-Id": "sess-1", gateway.AgentHeader: "copilot"})
			b, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if g := up.got(); resp.StatusCode != 200 || len(g) != 1 || !strings.HasSuffix(g[0].uri, "/v1/chat/completions") ||
				!strings.Contains(string(b), tc.wantOut) {
				t.Fatalf("status %d upstream %+v body %.200s", resp.StatusCode, g, b)
			}
			checkTranslatedLedger(t)
		})
	}
}

func checkTranslatedLedger(t *testing.T) {
	t.Helper()
	got := ledgerSettled(t, 1)
	if len(got) != 1 {
		t.Fatalf("ledger = %+v, want one translated response", got)
	}
	entry := got[0]
	entry.Ts, entry.Schema = time.Time{}, 0
	want := tokenledger.Entry{Profile: "oa", Provider: "openai", Model: "gpt-x", Session: "sess-1",
		Agent: "copilot", Input: 11, Output: 4}
	if entry != want {
		t.Fatalf("ledger = %+v, want %+v", entry, want)
	}
}

func TestIdleBodyCutsWedgedStream(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	b := newIdleBody(pr, 20*time.Millisecond)
	if _, err := b.Read(make([]byte, 1)); err == nil {
		t.Fatal("read on a silent body should fail after the idle cutoff")
	}
}

// A stream read failure before any output must permit failover instead of committing an empty 200.
func TestStreamReadErrorBeforeOutputFailsOver(t *testing.T) {
	up1 := newUpstream(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close()
	})
	up2 := newUpstream(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w,
			"data: {\"choices\":[{\"delta\":{\"content\":\"from-up2\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	})
	t.Setenv("UP1_KEY", "k1")
	t.Setenv("UP2_KEY", "k2")
	srv := setup(t, `{"default":"up1","fallback":["up2"],"providers":{"openai":{`+
		`"up1":{"protocol":"openai","base_url":"`+up1.URL+`","auth":{"type":"key","env":"UP1_KEY"},"models":{"opus":"m1"}},`+
		`"up2":{"protocol":"openai","base_url":"`+up2.URL+`","auth":{"type":"key","env":"UP2_KEY"},"models":{"opus":"m2"}}}}}`)

	resp := post(t, srv.URL+"/p/up1/v1/messages",
		`{"model":"m1","stream":true,"max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`, nil)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(b), "from-up2") || up2.hits.Load() != 1 {
		t.Fatalf("status %d body %q up2 hits %d: want 200 with from-up2 after failover, up2 hit once", resp.StatusCode,
			b, up2.hits.Load())
	}

	// up1 alone, with no fallback: the same early read error must surface as a 502, not an empty 200.
	srv2 := setup(t, `{"default":"up1","providers":{"openai":{`+
		`"up1":{"protocol":"openai","base_url":"`+up1.URL+`","auth":{"type":"key","env":"UP1_KEY"},"models":{"opus":"m1"}}}}}`)
	resp2 := post(t, srv2.URL+"/p/up1/v1/messages",
		`{"model":"m1","stream":true,"max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`, nil)
	b2, _ := io.ReadAll(resp2.Body)
	if resp2.StatusCode != http.StatusBadGateway || len(b2) == 0 {
		t.Fatalf("status %d body %q: want 502 with an error body, not an empty 200", resp2.StatusCode, b2)
	}
}

// stalledUploadUpstream accepts headers but never reads the body. A small receive
// buffer keeps the large request in the write phase, before ResponseHeaderTimeout.
func stalledUploadUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	up := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	up.Config.ConnState = func(conn net.Conn, state http.ConnState) {
		if state == http.StateNew {
			if err := conn.(*net.TCPConn).SetReadBuffer(1024); err != nil {
				t.Errorf("set stalled upstream receive buffer: %v", err)
			}
		}
	}
	up.Start()
	t.Cleanup(up.Close)
	t.Cleanup(func() { close(release) }) // unblock handlers before httptest waits for them.
	return up
}

func TestStalledLargeUploadFailsOverWithinAttemptBound(t *testing.T) {
	previous := uploadTimeout
	uploadTimeout = 2 * time.Second // covers the healthy leg too: proxy and upstream each parse the 16 MiB body
	t.Cleanup(func() { uploadTimeout = previous })
	stalled := stalledUploadUpstream(t)
	healthy := newUpstream(t, ok)
	t.Setenv("UP1_KEY", "k1")
	t.Setenv("UP2_KEY", "k2")
	var routed *Router
	setupRouter(t, failoverOverlay(stalled.URL, healthy.URL), func(r *Router) { routed = r })

	payload := `{"model":"m1-opus","messages":[{"role":"user","content":"` + strings.Repeat("x", 16<<20) + `"}]}`
	req := testRequest(t, http.MethodPost, "http://127.0.0.1/p/up1/v1/messages", strings.NewReader(payload))
	req.Header.Set("X-Api-Key", secret)
	var interruptedWrite atomic.Bool
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err != nil {
				interruptedWrite.Store(true)
			}
		},
	}))
	reply := httptest.NewRecorder()
	routed.ServeHTTP(reply, req)
	if healthy.hits.Load() != 1 || reply.Code != http.StatusOK ||
		!strings.Contains(reply.Body.String(), `"type":"message"`) {
		t.Fatalf("stalled upload did not fail over within attempt bound: healthy hits=%d status=%d body=%s",
			healthy.hits.Load(), reply.Code, reply.Body.String())
	}
	if !interruptedWrite.Load() {
		t.Fatal("upload completed before cancellation; regression did not exercise a stalled write")
	}
}
