// Package httpx is the one bounded-request helper: build, authorize, send, and read at most Limit bytes.
package httpx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
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

// Do sends c: status (0 = no response), up to Limit bytes, header; a read error or ErrTooLarge keeps the status.
func Do(ctx context.Context, c Call) (status int, body []byte, hdr http.Header, err error) {
	if c.Limit <= 0 {
		return 0, nil, nil, errors.New("httpx: Limit is required")
	}
	req, err := c.request(ctx)
	if err != nil {
		return 0, nil, nil, err
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	body, err = io.ReadAll(io.LimitReader(resp.Body, min(c.Limit, math.MaxInt64-1)+1))
	if err == nil && int64(len(body)) > c.Limit {
		body, err = body[:c.Limit], ErrTooLarge
	}
	return resp.StatusCode, body, resp.Header, err
}

// request builds c's request with Header copied and Authorize applied.
func (c Call) request(ctx context.Context) (*http.Request, error) {
	var rd io.Reader
	if len(c.Body) > 0 {
		rd = bytes.NewReader(c.Body)
	}
	req, err := http.NewRequestWithContext(ctx, c.Method, c.URL, rd)
	if err != nil {
		return nil, err
	}
	for k, v := range c.Header {
		req.Header[k] = append([]string(nil), v...)
	}
	if c.Authorize != nil {
		if err := c.Authorize(req.Header); err != nil {
			return nil, err
		}
	}
	return req, nil
}

// client is c.Client or a default-timeout one, with redirects off under NoRedirect.
func (c Call) client() *http.Client {
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: defaultTimeout, CheckRedirect: SameOriginRedirect}
	}
	if c.NoRedirect {
		cp := *client
		cp.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		client = &cp
	}
	return client
}

// SameOriginRedirect is a CheckRedirect that refuses a hop to another scheme, host or port or one that changes the
// method, so credentials in custom headers (X-Api-Key) never follow a redirect; the error names no query or userinfo.
func SameOriginRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	first := via[0]
	if req.URL.Scheme != first.URL.Scheme || req.URL.Host != first.URL.Host || req.Method != first.Method {
		safe := *req.URL
		safe.RawQuery, safe.Fragment, safe.User = "", "", nil
		return fmt.Errorf("refused redirect to %s: not the same origin and method", safe.String())
	}
	return nil
}
