// Package httpx is the one bounded-request helper: build, authorize, send, and read at most Limit bytes.
package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"time"

	"github.com/edotau/claude-code/internal/textutil"
)

// ErrTooLarge is returned with the first Limit bytes when a response body is longer than Call.Limit.
var ErrTooLarge = errors.New("response body too large")

// Client is the default bounded client; callers may supply Call.Client for different policies.
var Client = &http.Client{Timeout: 30 * time.Second, CheckRedirect: SameOriginRedirect}

// Call is one bounded request. Limit is required; a longer body is ErrTooLarge.
type Call struct {
	Client      *http.Client            // nil = a 30s client
	Method, URL string                  // Method "" = GET
	Header      http.Header             // copied, never mutated
	Authorize   func(http.Header) error // runs after Header is applied; its error is returned as is
	Body        []byte
	Limit       int64
	NoRedirect  bool // return a 3xx instead of following it
	Attempts    int  // zero or one means no retry
	Base        time.Duration
	Retry       func(int) bool    // statuses to retry
	Response    *[]byte           // DoJSON saves the bounded response when non-nil
	DecodeError func(error) error // optional mapping for JSON decoding failures
}

// Do sends c: status (0 = no response), up to Limit bytes, header; a read error or ErrTooLarge keeps the status.
func Do(ctx context.Context, c Call) (status int, body []byte, hdr http.Header, err error) {
	for attempt := 1; ; attempt++ {
		status, body, hdr, err = do(ctx, c)
		if status == 0 || attempt >= c.Attempts || c.Retry == nil || !c.Retry(status) {
			return status, body, hdr, err
		}
		if err := textutil.WaitRetry(ctx, attempt, c.Base, hdr, textutil.StatusError(status)); err != nil {
			return 0, nil, nil, &RetryError{err}
		}
	}
}

// RetryError preserves the interrupted wait and its last response status.
type RetryError struct{ Err error }

func (e *RetryError) Error() string { return e.Err.Error() }
func (e *RetryError) Unwrap() error { return e.Err }

// DoJSON reads a bounded response, saving it in Response if requested, and decodes a successful response into out.
func DoJSON(ctx context.Context, c Call, out any) (int, error) {
	status, body, _, err := Do(ctx, c)
	if c.Response != nil {
		*c.Response = body
	}
	if err == nil && status/100 == 2 && out != nil {
		err = json.Unmarshal(body, out)
		if err != nil && c.DecodeError != nil {
			err = c.DecodeError(err)
		}
	}
	return status, err
}

func do(ctx context.Context, c Call) (status int, body []byte, hdr http.Header, err error) {
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
		client = Client
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
