package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/gateway/translate"
	"github.com/edotau/claude-code/internal/providers"
)

// idleTimeout cuts a wedged upstream stream; a var so tests need not wait 10 minutes.
var idleTimeout = 10 * time.Minute

func relay(w http.ResponseWriter, resp *http.Response) {
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		if !hopHeaders[http.CanonicalHeaderKey(k)] {
			w.Header()[k] = vs
		}
	}
	w.WriteHeader(resp.StatusCode)
	flush := flusher(w)
	buf := make([]byte, 32<<10)
	var watch terminalWatch
	sse := strings.Contains(resp.Header.Get("Content-Type"), "event-stream")
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 && !forward(w, flush, buf[:n], sse, &watch) {
			return
		}
		if err == nil {
			continue
		}
		if !errors.Is(err, io.EOF) && !watch.done { // a drop after the terminal event cut nothing
			breakStream(w, err)
		}
		return
	}
}

// forward writes one upstream chunk to the client and feeds the terminal-event watch; false once the client is gone.
func forward(w http.ResponseWriter, flush func(), chunk []byte, sse bool, watch *terminalWatch) bool {
	if _, err := w.Write(chunk); err != nil {
		return false
	}
	flush()
	if sse {
		watch.see(chunk)
	}
	return true
}

// breakStream flags a mid-body upstream failure: the handler logs and meters first, then aborts the client stream.
func breakStream(w http.ResponseWriter, err error) {
	if sw, ok := w.(*statusWriter); ok {
		sw.broken = streamErrClass(err)
		return
	}
	panic(http.ErrAbortHandler)
}

// streamErrClass names a mid-body read error with one short word for the request log; the error text never is.
func streamErrClass(err error) string {
	var ne net.Error
	switch {
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		return "eof"
	case errors.Is(err, syscall.ECONNRESET):
		return "reset"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout(),
		errors.Is(err, net.ErrClosed), errors.Is(err, http.ErrBodyReadAfterClose): // idleBody closes a wedged body
		return "timeout"
	}
	return "other"
}

// terminalWatch notes whether a complete terminal SSE frame went by; a frame over frameCap is dropped as not terminal.
type terminalWatch struct {
	line     []byte // partial line carried across reads
	data     []byte // the current frame's data, kept only when it has no event: line
	event    string
	hasEvent bool
	hasData  bool
	over     bool // the current frame exceeded frameCap
	skipLine bool // the current line exceeded frameCap: ignore its rest
	done     bool
}

const frameCap = 64 << 10

func (t *terminalWatch) see(p []byte) {
	for len(p) > 0 && !t.done {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			t.partial(p)
			return
		}
		line := p[:i]
		if len(t.line) > 0 || t.skipLine {
			t.partial(line)
			line = t.line
		}
		if !t.skipLine {
			t.handle(line)
		}
		t.line, t.skipLine = t.line[:0], false
		p = p[i+1:]
	}
}

// partial appends an unfinished line piece, marking the frame over-cap once the line outgrows frameCap.
func (t *terminalWatch) partial(b []byte) {
	if t.skipLine {
		return
	}
	if len(t.line)+len(b) > frameCap {
		t.over, t.skipLine, t.line = true, true, t.line[:0]
		return
	}
	t.line = append(t.line, b...)
}

// handle reads one SSE line; a blank line closes the frame and judges it.
func (t *terminalWatch) handle(line []byte) {
	line = bytes.TrimSuffix(line, []byte{'\r'})
	switch {
	case len(line) == 0:
		if t.frameTerminal() {
			t.done = true
		}
		t.event, t.hasEvent, t.hasData, t.over, t.data = "", false, false, false, t.data[:0]
	case bytes.HasPrefix(line, []byte("event:")):
		t.event, t.hasEvent = strings.TrimSpace(string(line[6:])), true
	case bytes.HasPrefix(line, []byte("data:")) && !t.hasEvent && !t.over:
		v := bytes.TrimPrefix(line[5:], []byte{' '})
		if len(t.data)+len(v)+1 > frameCap {
			t.over, t.data = true, t.data[:0]
			return
		}
		if t.hasData {
			t.data = append(t.data, '\n')
		}
		t.data, t.hasData = append(t.data, v...), true
	}
}

// frameTerminal judges a complete frame: its event: type where one is sent, else [DONE] or its data's top-level type.
func (t *terminalWatch) frameTerminal() bool {
	if t.over {
		return false
	}
	if t.hasEvent {
		return terminalType(t.event)
	}
	if string(t.data) == "[DONE]" {
		return true
	}
	if !bytes.Contains(t.data, []byte("response.")) && !bytes.Contains(t.data, []byte("message_stop")) {
		return false // cheap prefilter: a terminal type name must appear in the data
	}
	var f struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(t.data, &f) == nil && terminalType(f.Type)
}

func terminalType(s string) bool {
	switch s {
	case "response.completed", "response.incomplete", "response.failed", "message_stop":
		return true
	}
	return false
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
}

func flusher(w http.ResponseWriter) func() {
	if f, ok := w.(http.Flusher); ok {
		return f.Flush
	}
	return func() {}
}

func writeJSON(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, translate.ErrorBody(status, msg))
}

type statusWriter struct {
	http.ResponseWriter
	status int
	rc     *http.ResponseController // cached: constructing one per Flush call allocated on every SSE chunk
	errBuf []byte                   // the first bytes of an error reply (status >= 400), for the request log
	broken string                   // error class when relay hit a mid-body upstream failure: abort once logged and metered
}

// brokenDiag marks a request line whose upstream reply broke mid-body, with the error class.
func brokenDiag(sw *statusWriter) string {
	if sw.broken != "" {
		return " stream=broken:" + sw.broken
	}
	return ""
}

// abortIfBroken breaks the client's stream (http.ErrAbortHandler) when relay saw the upstream fail mid-body.
func (s *statusWriter) abortIfBroken() {
	if s.broken != "" {
		panic(http.ErrAbortHandler)
	}
}

// errBufMax bounds how much of an error reply the request log may read its code and message from.
const errBufMax = 4 << 10

func (s *statusWriter) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	if s.status >= http.StatusBadRequest && len(s.errBuf) < errBufMax {
		s.errBuf = append(s.errBuf, b[:min(len(b), errBufMax-len(s.errBuf))]...)
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusWriter) Flush() { _ = s.rc.Flush() }

// idleBody closes the upstream body when no Read completes within idle, unblocking a wedged stream.
type idleBody struct {
	rc    io.ReadCloser
	idle  time.Duration
	timer *time.Timer
}

func newIdleBody(rc io.ReadCloser, idle time.Duration) *idleBody {
	return &idleBody{rc: rc, idle: idle, timer: time.AfterFunc(idle, func() { _ = rc.Close() })}
}

func (b *idleBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	b.timer.Reset(b.idle)
	return n, err
}

func (b *idleBody) Close() error {
	b.timer.Stop()
	return b.rc.Close()
}

func (r *Router) relayChat(w *statusWriter, in *http.Request, resp *http.Response, t providers.Target,
	clientModel string, stream bool) (bool, error) {
	if !stream {
		return relayChatBuffered(w, in, resp.Body, t, clientModel)
	}
	if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "json") { // stream ignored: SSE parsing would see EOF
		return false, fmt.Errorf("upstream answered a stream request with %s, not text/event-stream", ct)
	}
	// Per-token providers tee the stream's tail so the ledger can scrape usage without changing translate's API.
	var tail *tailBuffer
	body := io.Reader(resp.Body)
	if metered(t.Provider) {
		tail = &tailBuffer{}
		body = io.TeeReader(resp.Body, tail)
	}
	return r.relayChatStream(w, in, body, tail, t, clientModel)
}

// relayChatStream translates an SSE chat stream; false and the error only when nothing was written yet.
func (r *Router) relayChatStream(w *statusWriter, in *http.Request, body io.Reader, tail *tailBuffer,
	t providers.Target, clientModel string) (bool, error) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	// No explicit 200: a read error before the first event writes nothing, so the caller can still fail over.
	err := translate.StreamChat(w, flusher(w), body, clientModel)
	if tail != nil {
		logLedgerUsage(in, t.Provider, t.Model, scrapeChatUsage(tail.buf))
	}
	if err != nil && w.status == 0 {
		return false, err
	}
	if err != nil && in.Context().Err() == nil {
		fmt.Fprintf(r.log, "proxy: %s stream translation: %v\n", t.Provider.Name, err)
	}
	return true, nil
}

// relayChatBuffered reads a whole chat reply, translates it, and writes it as one JSON response.
func relayChatBuffered(w *statusWriter, in *http.Request, body io.Reader, t providers.Target,
	clientModel string) (bool, error) {
	b, err := io.ReadAll(io.LimitReader(body, maxBody+1))
	if err != nil {
		return false, err
	}
	if len(b) > maxBody {
		return false, fmt.Errorf("upstream reply exceeds %d bytes", maxBody)
	}
	if metered(t.Provider) {
		logLedgerUsage(in, t.Provider, t.Model, scrapeChatUsage(b))
	}
	ab, err := translate.FromChatResponse(b, clientModel)
	if err != nil {
		return false, err
	}
	writeJSON(w, http.StatusOK, ab)
	return true, nil
}

func (r *Router) sendRequest(in *http.Request, method string, p *providers.Provider, url string, body []byte,
	hdr func(http.Header), credential string) (*http.Response, error) {
	ctx := in.Context()
	if n, ok := ctx.Value(attemptsKey{}).(*int); ok {
		*n++
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hdr(req.Header)
	r.attribute(req.Header, in, p)
	if credential == "" {
		if err := providers.Authorize(ctx, p, req.Header); err != nil {
			return nil, err
		}
	} else {
		for k, v := range p.Headers {
			req.Header.Set(k, v)
		}
		providers.SetCredential(p, req.Header, credential)
	}
	resp, err := r.doBounded(req)
	if err != nil {
		return nil, &fallbackError{error: err}
	}
	return resp, nil
}

// uploadTimeout bounds request write plus wait for response headers; a var so tests need not wait minutes.
var uploadTimeout = 5 * time.Minute

// doBounded sends req and cancels it if headers, upload included, take longer than uploadTimeout.
func (r *Router) doBounded(req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancel(req.Context())
	timer := time.AfterFunc(uploadTimeout, cancel)
	resp, err := r.client.Do(req.WithContext(ctx))
	timedOut := !timer.Stop()
	if timedOut {
		cancel()
		if resp != nil {
			_ = resp.Body.Close()
		}
		return nil, fmt.Errorf("upstream sent no response within %s", uploadTimeout)
	}
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = cancelOnClose{resp.Body, cancel} // the body streams past the deadline, so cancel only on Close
	return resp, nil
}

// cancelOnClose releases the request context once the response body is closed.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c cancelOnClose) Close() error {
	defer c.cancel()
	return c.ReadCloser.Close()
}
