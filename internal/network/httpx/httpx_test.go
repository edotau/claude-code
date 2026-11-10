package httpx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDoReturnsStatusBodyHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Seen", r.Header.Get("X-In")+r.Method)
		w.WriteHeader(http.StatusTeapot)
		w.Write([]byte("short"))
	}))
	defer srv.Close()
	st, body, hdr, err := Do(context.Background(), Call{URL: srv.URL, Header: http.Header{"X-In": {"a"}}, Limit: 10})
	if err != nil || st != http.StatusTeapot || string(body) != "short" || hdr.Get("X-Seen") != "aGET" {
		t.Fatalf("got %d %q %v %v", st, body, hdr, err)
	}
}

func TestDoBodyOverLimitIsErrTooLarge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(strings.Repeat("x", 11)))
	}))
	defer srv.Close()
	st, body, _, err := Do(context.Background(), Call{URL: srv.URL, Limit: 10})
	if !errors.Is(err, ErrTooLarge) || st != 200 || len(body) != 10 {
		t.Fatalf("got %d len=%d %v", st, len(body), err)
	}
	if _, body, _, err = Do(context.Background(), Call{URL: srv.URL, Limit: 11}); err != nil || len(body) != 11 {
		t.Fatalf("exact limit: len=%d %v", len(body), err)
	}
}

func TestDoAuthorizeErrorSendsNothing(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer srv.Close()
	want := errors.New("no credential")
	st, _, _, err := Do(context.Background(), Call{URL: srv.URL, Limit: 10, Authorize: func(http.Header) error { return want }})
	if err != want || st != 0 || hits.Load() != 0 {
		t.Fatalf("err=%v status=%d hits=%d", err, st, hits.Load())
	}
}

func TestDoAuthorizeRunsAfterHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(r.Header.Get("X-A")))
	}))
	defer srv.Close()
	h := http.Header{"X-A": {"old"}}
	_, body, _, err := Do(context.Background(), Call{URL: srv.URL, Header: h, Limit: 10,
		Authorize: func(h http.Header) error { h.Set("X-A", h.Get("X-A")+"+auth"); return nil }})
	if err != nil || string(body) != "old+auth" || h.Get("X-A") != "old" {
		t.Fatalf("body=%q err=%v caller header=%v", body, err, h)
	}
}

func TestDoNoRedirectReturnsThe3xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/next" {
			w.Write([]byte("followed"))
			return
		}
		http.Redirect(w, r, "/next", http.StatusFound)
	}))
	defer srv.Close()
	st, _, hdr, err := Do(context.Background(), Call{URL: srv.URL, Limit: 1000, NoRedirect: true})
	if err != nil || st != http.StatusFound || hdr.Get("Location") != "/next" {
		t.Fatalf("got %d %v %v", st, hdr, err)
	}
	if st, body, _, _ := Do(context.Background(), Call{URL: srv.URL, Limit: 10}); st != 200 || string(body) != "followed" {
		t.Fatalf("default follows: %d %q", st, body)
	}
}

func TestDoContextCancel(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if st, _, _, err := Do(ctx, Call{URL: srv.URL, Limit: 10}); err == nil || st != 0 {
		t.Fatalf("status=%d err=%v", st, err)
	}
}

func TestDoNilClientGetsTheDefaultTimeout(t *testing.T) {
	if defaultTimeout != 30*time.Second {
		t.Fatalf("default timeout %v", defaultTimeout)
	}
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	orig := defaultTimeout
	defaultTimeout = 50 * time.Millisecond
	t.Cleanup(func() { defaultTimeout = orig })
	start := time.Now()
	if _, _, _, err := Do(context.Background(), Call{URL: srv.URL, Limit: 10}); err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("nil client did not time out: %v", err)
	}
}
