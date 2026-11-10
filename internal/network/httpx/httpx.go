// Package httpx is the one bounded-request helper: build, authorize, send, and read at most Limit bytes.
package httpx

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"time"
)

// ErrTooLarge is returned with the first Limit bytes when a response body is longer than Call.Limit.
var ErrTooLarge = errors.New("response body too large")

// defaultTimeout is the whole-request limit of the client a nil Call.Client gets.
var defaultTimeout = 30 * time.Second

// Call is one bounded request. Limit is required; a longer body is ErrTooLarge.
type Call struct {
	Client      *http.Client            // nil = a 30s client
	Method, URL string                  // Method "" = GET
	Header      http.Header             // copied, never mutated
	Authorize   func(http.Header) error // runs after Header is applied; its error is returned as is
	Body        []byte
	Limit       int64
	NoRedirect  bool // return a 3xx instead of following it
}

// Do sends c under ctx and returns the status, at most Limit bytes and the response header. Status 0 means no
// response arrived (build, Authorize or transport error); otherwise a read error or ErrTooLarge comes with the
// status and the bytes read so far. No retry.
func Do(ctx context.Context, c Call) (status int, body []byte, hdr http.Header, err error) {
	if c.Limit <= 0 {
		return 0, nil, nil, errors.New("httpx: Limit is required")
	}
	var rd io.Reader
	if len(c.Body) > 0 {
		rd = bytes.NewReader(c.Body)
	}
	req, err := http.NewRequestWithContext(ctx, c.Method, c.URL, rd)
	if err != nil {
		return 0, nil, nil, err
	}
	for k, v := range c.Header {
		req.Header[k] = append([]string(nil), v...)
	}
	if c.Authorize != nil {
		if err := c.Authorize(req.Header); err != nil {
			return 0, nil, nil, err
		}
	}
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: defaultTimeout}
	}
	if c.NoRedirect {
		cp := *client
		cp.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		client = &cp
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	body, err = io.ReadAll(io.LimitReader(resp.Body, c.Limit+1))
	if err == nil && int64(len(body)) > c.Limit {
		body, err = body[:c.Limit], ErrTooLarge
	}
	return resp.StatusCode, body, resp.Header, err
}
