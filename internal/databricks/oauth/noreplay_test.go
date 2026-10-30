package oauth

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// dropAfterRead returns a token server that reads the whole body, counts the POST, then kills the connection.
func dropAfterRead(t *testing.T, posts *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		posts.Add(1)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		_ = conn.Close()
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRefreshTokenNotReplayedAfterSend(t *testing.T) {
	var posts atomic.Int32
	srv := dropAfterRead(t, &posts)
	if _, err := RefreshU2M(context.Background(), srv.URL, "refresh-1"); err == nil {
		t.Fatal("dropped response reported success")
	}
	if n := posts.Load(); n != 1 {
		t.Fatalf("single-use refresh token POSTed %d times, want 1", n)
	}
}

func TestAuthorizationCodeNotReplayedAfterSend(t *testing.T) {
	var posts atomic.Int32
	srv := dropAfterRead(t, &posts)
	errc := make(chan error, 1)
	open := drive(t, "the-code", same, errc)
	_, err := LoginU2M(context.Background(), srv.URL, LoginOptions{Open: open, Port: freePort(t), Timeout: 10 * time.Second})
	if err == nil {
		t.Fatal("dropped response reported success")
	}
	<-errc
	if n := posts.Load(); n != 1 {
		t.Fatalf("single-use authorization code POSTed %d times, want 1", n)
	}
}

// Contrast: client-credentials is idempotent, so the same drop IS retried.
func TestClientCredentialsRetriedAfterDrop(t *testing.T) {
	var posts atomic.Int32
	srv := dropAfterRead(t, &posts)
	if _, err := MintM2M(context.Background(), srv.URL, "id", "s"); err == nil {
		t.Fatal("dropped response reported success")
	}
	if n := posts.Load(); n < 2 {
		t.Fatalf("idempotent grant POSTed %d times, want a retry", n)
	}
}

func TestListenLoopbackBindsLoopbackOnly(t *testing.T) {
	ln, port, err := listenLoopback(freePort(t))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	c, err := net.Dial("tcp4", net.JoinHostPort("127.0.0.1", itoa(port)))
	if err != nil {
		t.Fatalf("127.0.0.1 not served: %v", err)
	}
	c.Close()
	if c6, err := net.Dial("tcp6", net.JoinHostPort("::1", itoa(port))); err == nil {
		c6.Close() // IPv6 loopback present: it must be served too
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// countingTransport counts the attempts that reach the transport.
type countingTransport struct{ n atomic.Int32 }

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.n.Add(1)
	return http.DefaultTransport.RoundTrip(r)
}

func TestRefreshRetriesWhenNothingWasWritten(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // dial refused: the request is never written
	ct := &countingTransport{}
	orig, origBackoff := tokenClient, RetryBackoff
	tokenClient, RetryBackoff = &http.Client{Transport: ct, Timeout: 5 * time.Second}, time.Millisecond
	t.Cleanup(func() { tokenClient, RetryBackoff = orig, origBackoff })
	if _, err := RefreshU2M(context.Background(), url, "refresh-1"); err == nil {
		t.Fatal("refused dial reported success")
	}
	if n := ct.n.Load(); n != maxAttempts {
		t.Fatalf("unwritten one-shot grant attempted %d times, want %d", n, maxAttempts)
	}
}
