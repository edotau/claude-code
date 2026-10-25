package sdkport

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"syscall"
	"time"
)

const (
	defaultPort     = 8020 // the CLI's registered callback port; the SDK falls back upward to maxPort
	maxPort         = 8040
	defaultTimeout  = 5 * time.Minute
	callbackTimeout = 5 * time.Second
)

// LoginOptions tunes LoginU2M; the zero value is the interactive default.
type LoginOptions struct {
	// Open launches the browser; nil or an error prints the URL to Out instead (headless).
	Open func(authURL string) error
	// Out receives the headless URL prompt (default os.Stderr).
	Out io.Writer
	// Timeout bounds the whole login (default 5 minutes).
	Timeout time.Duration
	// Port is the first localhost callback port to try (default 8020, falling back upward to 8040).
	Port int
}

type callbackResult struct {
	code string
	err  error
}

func randB64(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// listenLoopback binds port p on 127.0.0.1 and, when IPv6 is up, on ::1 too (localhost may resolve to either),
// trying first..maxPort; a port held on either address is skipped.
func listenLoopback(first int) (net.Listener, int, error) {
	if first <= 0 {
		first = defaultPort
	}
	last := max(maxPort, first)
	for p := first; p <= last; p++ {
		ln4, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)))
		if err != nil {
			continue
		}
		ln6, err := net.Listen("tcp6", net.JoinHostPort("::1", strconv.Itoa(p)))
		switch {
		case err == nil:
			return newDualListener(ln4, ln6), p, nil
		case errors.Is(err, syscall.EADDRINUSE):
			_ = ln4.Close() // another process answers localhost on ::1 at this port
			continue
		}
		return ln4, p, nil // no IPv6 loopback: 127.0.0.1 alone
	}
	if first == last {
		return nil, 0, fmt.Errorf("oauth: login callback port %d on localhost is busy (another login running?)", first)
	}
	return nil, 0, fmt.Errorf("oauth: login callback ports %d-%d on localhost are all busy (another login running?)",
		first, last)
}

// dualListener accepts on several listeners as one; Addr is the first (IPv4) one.
type dualListener struct {
	lns   []net.Listener
	conns chan net.Conn
	errs  chan error
	done  chan struct{}
	once  sync.Once
}

func newDualListener(lns ...net.Listener) *dualListener {
	d := &dualListener{lns: lns, conns: make(chan net.Conn), errs: make(chan error, len(lns)), done: make(chan struct{})}
	for _, ln := range lns {
		go d.loop(ln)
	}
	return d
}

func (d *dualListener) loop(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			select {
			case d.errs <- err:
			case <-d.done:
			}
			return
		}
		select {
		case d.conns <- c:
		case <-d.done:
			_ = c.Close()
			return
		}
	}
}

func (d *dualListener) Accept() (net.Conn, error) {
	select {
	case c := <-d.conns:
		return c, nil
	case err := <-d.errs:
		return nil, err
	case <-d.done:
		return nil, net.ErrClosed
	}
}

func (d *dualListener) Close() error {
	d.once.Do(func() {
		close(d.done)
		for _, ln := range d.lns {
			_ = ln.Close()
		}
	})
	return nil
}

func (d *dualListener) Addr() net.Addr { return d.lns[0].Addr() }

// buildAuthorizeURL is the browser URL for the authorization-code + PKCE (S256) request.
func buildAuthorizeURL(host, redirect, state, challenge string) string {
	q := url.Values{
		"client_id":             {ClientID},
		"response_type":         {"code"},
		"redirect_uri":          {redirect},
		"scope":                 {scopeU2M},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	return authorizeURL(host) + "?" + q.Encode()
}

// LoginU2M runs the browser authorization-code + PKCE flow against host and returns access and refresh tokens.
func LoginU2M(ctx context.Context, host string, opts LoginOptions) (Token, error) {
	h, err := normHost(host)
	if err != nil {
		return Token{}, err
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	verifier, err := randB64(32)
	if err != nil {
		return Token{}, fmt.Errorf("oauth: login at %s: %w", h, err)
	}
	state, err := randB64(16)
	if err != nil {
		return Token{}, fmt.Errorf("oauth: login at %s: %w", h, err)
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	ln, port, err := listenLoopback(opts.Port)
	if err != nil {
		return Token{}, err
	}
	redirect := "http://localhost:" + strconv.Itoa(port)
	results := make(chan callbackResult, 1)
	srv := &http.Server{Handler: callbackHandler(h, state, results), ReadHeaderTimeout: callbackTimeout}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		sctx, c := context.WithTimeout(context.Background(), callbackTimeout)
		defer c()
		_ = srv.Shutdown(sctx)
	}()

	authURL := buildAuthorizeURL(h, redirect, state, challenge)
	if opts.Open == nil || opts.Open(authURL) != nil {
		out := opts.Out
		if out == nil {
			out = os.Stderr
		}
		fmt.Fprintf(out, "Open this URL in a browser to sign in to %s:\n%s\n", h, authURL)
	}

	select {
	case <-ctx.Done():
		return Token{}, fmt.Errorf("oauth: login at %s: waiting for the browser callback: %w", h, ctx.Err())
	case r := <-results:
		if r.err != nil {
			return Token{}, r.err
		}
		form := url.Values{
			"grant_type":    {"authorization_code"},
			"client_id":     {ClientID},
			"code":          {r.code},
			"code_verifier": {verifier},
			"redirect_uri":  {redirect},
		}
		tok, err := postToken(ctx, h, "authorization-code exchange", form, "", "", retryOneShot)
		if err != nil {
			return Token{}, err
		}
		if tok.Refresh == "" {
			return Token{}, fmt.Errorf("oauth: login at %s: no refresh token issued (offline_access not granted)", h)
		}
		return tok, nil
	}
}

// callbackHandler accepts exactly one valid callback; a wrong-state request is rejected without consuming the slot.
func callbackHandler(host, state string, out chan<- callbackResult) http.Handler {
	var mu sync.Mutex
	used := false
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") == "" && q.Get("code") == "" && q.Get("error") == "" {
			http.NotFound(w, r) // favicon and stray probes
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if used {
			http.Error(w, "login already completed", http.StatusGone)
			return
		}
		if q.Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			return
		}
		used = true
		var res callbackResult
		switch {
		case q.Get("error") != "":
			res.err = &oauthError{Host: host, Op: "login", Code: q.Get("error"), Desc: q.Get("error_description"), Status: http.StatusBadRequest}
			http.Error(w, "Login failed. You can close this tab.", http.StatusBadRequest)
		case q.Get("code") == "":
			res.err = errors.New("oauth: login at " + host + ": callback carried no authorization code")
			http.Error(w, "Login failed. You can close this tab.", http.StatusBadRequest)
		default:
			res.code = q.Get("code")
			fmt.Fprintln(w, "Login complete. You can close this tab.")
		}
		out <- res
	})
}
