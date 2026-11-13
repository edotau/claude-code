package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"syscall"
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

// stream=broken marks only a Responses stream cut before its terminal event, not a drop right after response.completed.
func TestRelayBrokenMarkerFollowsTerminalEvent(t *testing.T) {
	delta := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n"
	done := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":3}}}\n\n"
	for name, tc := range map[string]struct {
		body   string
		broken bool
	}{
		"drop-after-completed": {delta + done, false},
		"cut-before-completed": {delta, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			up := newRawUp(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, tc.body)
				w.(http.Flusher).Flush()
				conn, _, _ := w.(http.Hijacker).Hijack() // closes with no final chunk: the proxy reads an unexpected EOF
				conn.Close()
			})
			t.Setenv("PT_KEY", "prov-cred")
			logs := &lockedBuf{}
			srv := setupRouter(t, ptOverlay(up.URL), func(r *Router) { r.log = logs })
			resp := ptDo(t, http.MethodPost, srv.URL+"/p/pt/responses/responses", `{"model":"m","stream":true}`, nil)
			b, err := io.ReadAll(resp.Body)
			if !strings.Contains(string(b), "response.output_text.delta") {
				t.Fatalf("client body = %q (err %v), want the relayed events", b, err)
			}
			if got := strings.Contains(logs.String(), " stream=broken:eof\n"); got != tc.broken {
				t.Errorf("stream=broken:eof = %t, want %t; log %q", got, tc.broken, logs.String())
			}
			if (err != nil) != tc.broken {
				t.Errorf("client read err = %v, want broken=%t", err, tc.broken)
			}
		})
	}
}

// streamErrClass maps a read error to one short word and never carries the error text.
func TestStreamErrClass(t *testing.T) {
	for want, err := range map[string]error{
		"eof":      fmt.Errorf("read: %w", io.ErrUnexpectedEOF),
		"reset":    fmt.Errorf("read tcp 10.0.0.1:1->10.0.0.2:2: %w", syscall.ECONNRESET),
		"canceled": context.Canceled,
		"timeout":  context.DeadlineExceeded,
		"other":    errors.New("secret upstream text"),
	} {
		if got := streamErrClass(err); got != want {
			t.Errorf("streamErrClass(%v) = %q, want %q", err, got, want)
		}
	}
	if got := streamErrClass(net.ErrClosed); got != "timeout" {
		t.Errorf("closed body = %q, want timeout", got)
	}
}

// A terminal marker split across two reads is still seen; the same text escaped inside a delta is not.
func TestTerminalWatchSplitAndEscapedMarkers(t *testing.T) {
	var split terminalWatch
	split.see([]byte(`data: {"type":"response.comp`))
	split.see([]byte("leted\",\"response\":{}}\n\n"))
	if !split.done {
		t.Error("split response.completed not seen")
	}
	var text terminalWatch
	text.see([]byte(`data: {"delta":"say \"response.completed\" now"}`))
	if text.done {
		t.Error("escaped marker inside a delta counted as terminal")
	}
}

// Finding 1: the marker text as a payload value of a non-terminal event is not a finished stream.
func TestTerminalWatchIgnoresMarkerInPayloadValue(t *testing.T) {
	var w terminalWatch
	w.see([]byte("data: {\"type\":\"content_block_delta\",\"delta\":\"message_stop\"}\n\n"))
	w.see([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"response.completed\"}\n\n"))
	if w.done {
		t.Fatal("marker text inside a payload value counted as the terminal event")
	}
}

// Finding 2: a terminal event cut off before its blank line is not a finished stream; the completed frame is.
func TestTerminalWatchNeedsCompleteFrame(t *testing.T) {
	var w terminalWatch
	w.see([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n"))
	if w.done {
		t.Fatal("a terminal frame without its blank line counted as finished")
	}
	w.see([]byte("\n"))
	if !w.done {
		t.Fatal("a complete message_stop frame was not seen")
	}
}

// A frame over the cap is dropped as not terminal and memory stays bounded; the next frame still counts.
func TestTerminalWatchOverCapFrameNotTerminal(t *testing.T) {
	var w terminalWatch
	w.see([]byte("data: {\"type\":\"x\",\"pad\":\"" + strings.Repeat("a", 2*frameCap) + "\",\"z\":\"message_stop\"}\n\n"))
	if w.done || cap(w.data) > 2*frameCap {
		t.Fatalf("over-cap frame: done=%t cap=%d", w.done, cap(w.data))
	}
	w.see([]byte("data: [DONE]\n\n"))
	if !w.done {
		t.Fatal("[DONE] after an over-cap frame not seen")
	}
}

// Frame shapes a stream really sends: empty and comment frames, CRLF, splits at the blank line, the line cap edges.
func TestTerminalWatchFrameShapes(t *testing.T) {
	// frame is one data-only terminal frame whose "data: ..." line is exactly lineLen bytes, then its blank line.
	frame := func(lineLen int) string {
		const head, tail = `data: {"type":"message_stop","p":"`, `"}`
		return head + strings.Repeat("a", lineLen-len(head)-len(tail)) + tail + "\n\n"
	}
	cut := func(s string, n int) []string { return []string{s[:n], s[n:]} }
	for _, tc := range []struct {
		name   string
		chunks []string
		done   bool
	}{
		{"empty frame", []string{"\n\n"}, false},
		{"comment-only frames", []string{": ping\n\n: pong\n\n"}, false},
		{"comment line inside a terminal frame", []string{": c\nevent: message_stop\n\n"}, true},
		{"CRLF terminal frame", []string{"event: message_stop\r\ndata: {}\r\n\r\n"}, true},
		{"CRLF split at the blank line", []string{"data: [DONE]\r\n", "\r\n"}, true},
		{"CRLF split inside the blank line", []string{"data: [DONE]\r\n\r", "\n"}, true},
		{"CRLF blank line never finished", []string{"data: [DONE]\r\n\r"}, false},
		{"one read, data one byte under the cap", []string{frame(frameCap + 5)}, true},
		{"one read, data one byte over the cap", []string{frame(frameCap + 7)}, false},
		{"line split across reads, exactly at the cap", cut(frame(frameCap), 10), true},
		{"line split across reads, one byte over", cut(frame(frameCap+1), 10), false},
		{"next frame counts after an over-cap split line", append(cut(frame(frameCap+1), 10), "data: [DONE]\n\n"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var w terminalWatch
			for _, c := range tc.chunks {
				w.see([]byte(c))
			}
			if w.done != tc.done {
				t.Errorf("done = %t, want %t", w.done, tc.done)
			}
		})
	}
}

func BenchmarkTerminalWatchSee(b *testing.B) {
	chunk := []byte(strings.Repeat("data: {\"delta\":\"xxxxxxxxxxxxxxxx\"}\n\n", 32<<10/40))
	b.SetBytes(int64(len(chunk)))
	for b.Loop() {
		var w terminalWatch
		w.see(chunk)
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
