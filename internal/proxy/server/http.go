package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/translate"
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
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			flush()
		}
		if err != nil {
			return
		}
	}
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
	writeJSON(w, status, errorBody(status, msg))
}

// errorBody is the Anthropic error envelope Claude Code parses.
func errorBody(status int, msg string) []byte {
	typ := "api_error"
	switch status {
	case 400, 413:
		typ = "invalid_request_error"
	case 401:
		typ = "authentication_error"
	case 403:
		typ = "permission_error"
	case 404:
		typ = "not_found_error"
	case 429:
		typ = "rate_limit_error"
	case 529:
		typ = "overloaded_error"
	}
	b, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]string{"type": typ, "message": msg}})
	return b
}

type statusWriter struct {
	http.ResponseWriter
	status int
	rc     *http.ResponseController // cached: constructing one per Flush call allocated on every SSE chunk
}

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

func (r *Router) relayChat(w *statusWriter, in *http.Request, resp *http.Response, t providers.Target, clientModel string, stream bool) (bool, error) {
	p := t.Provider
	// Per-token providers tee raw bytes so the ledger can scrape usage without changing translate's API.
	var capture *bytes.Buffer
	body2 := io.Reader(resp.Body)
	if metered(p) {
		capture = &bytes.Buffer{}
		body2 = io.TeeReader(resp.Body, capture)
	}
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		// No explicit 200: a read error before the first event writes nothing, so the caller can still fail over.
		err := translate.StreamChat(w, flusher(w), body2, clientModel)
		if capture != nil {
			logLedgerUsage(in, p, t.Model, scrapeChatUsage(capture.Bytes()))
		}
		if err != nil && w.status == 0 {
			return false, err
		}
		if err != nil && in.Context().Err() == nil {
			fmt.Fprintf(r.log, "proxy: %s stream translation: %v\n", p.Name, err)
		}
		return true, nil
	}
	b, err := io.ReadAll(io.LimitReader(body2, maxBody))
	if err != nil {
		return false, err
	}
	if capture != nil {
		logLedgerUsage(in, p, t.Model, scrapeChatUsage(capture.Bytes()))
	}
	ab, err := translate.FromChatResponse(b, clientModel)
	if err != nil {
		return false, err
	}
	writeJSON(w, http.StatusOK, ab)
	return true, nil
}

func (r *Router) sendRequest(in *http.Request, method string, p *providers.Provider, url string, body []byte, hdr func(http.Header), credential string) (*http.Response, error) {
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
	resp, err := r.client.Do(req)
	if err != nil {
		return resp, &fallbackError{error: err}
	}
	return resp, nil
}
