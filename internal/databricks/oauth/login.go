package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
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
	authorizePath = "/oidc/v1/authorize"
	defaultPort   = 8020
	portSpan      = 20
	loginTimeout  = 5 * time.Minute
	failedPage    = "Login failed. You can close this tab."
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

// loopback serves one callback port on every loopback family the host has.
type loopback struct{ ls []net.Listener }

func (l *loopback) Close() error {
	var err error
	for _, ln := range l.ls {
		err = errors.Join(err, ln.Close())
	}
	return err
}

func (l *loopback) Addr() net.Addr { return l.ls[0].Addr() }

// listenLoopback binds 127.0.0.1 (and ::1 when the host has it) on the first free port from first to first+portSpan.
func listenLoopback(first int) (*loopback, int, error) {
	for port := first; port <= first+portSpan; port++ {
		v4, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			continue
		}
		lb := &loopback{ls: []net.Listener{v4}}
		v6, err := net.Listen("tcp6", net.JoinHostPort("::1", strconv.Itoa(port)))
		switch {
		case err == nil:
			lb.ls = append(lb.ls, v6)
		case errors.Is(err, syscall.EADDRINUSE):
			_ = v4.Close()
			continue
		}
		return lb, port, nil
	}
	return nil, 0, fmt.Errorf("oauth: login callback port %d on localhost is busy (another login running?)", first)
}

// randomB64 returns n random bytes, base64url without padding.
func randomB64(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// callback is the browser redirect target: it accepts one request carrying the expected state.
type callback struct {
	state string
	mu    sync.Mutex
	done  bool
	res   chan callbackResult
}

type callbackResult struct {
	code string
	err  *oauthError // set when the authorization server reported an error
	miss bool        // the redirect carried neither code nor error
}

// classify turns a state-checked redirect into its result.
func classify(q url.Values) callbackResult {
	res := callbackResult{code: q.Get("code")}
	if e := q.Get("error"); e != "" {
		res.err = &oauthError{Op: "login", Code: e, Desc: q.Get("error_description"), Status: http.StatusBadRequest}
	} else if res.code == "" {
		res.miss = true
	}
	return res
}

func (c *callback) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.done {
		http.Error(w, "Login already finished.", http.StatusGone)
		return
	}
	q := r.URL.Query()
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(c.state)) != 1 {
		http.Error(w, failedPage, http.StatusBadRequest)
		return
	}
	c.done = true
	res := classify(q)
	if res.err != nil || res.miss {
		http.Error(w, failedPage, http.StatusBadRequest)
	} else {
		_, _ = io.WriteString(w, "Login complete. You can close this tab.\n")
	}
	c.res <- res
}

// authorizeURL builds the PKCE authorization request for the loopback redirect.
func authorizeURL(host, redirect, state, verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return host + authorizePath + "?" + url.Values{
		"client_id": {ClientID}, "response_type": {"code"}, "redirect_uri": {redirect}, "scope": {"all-apis offline_access"},
		"state": {state}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"},
	}.Encode()
}

// await blocks for the browser redirect and returns its authorization code.
func await(ctx context.Context, h string, cb *callback) (string, error) {
	select {
	case res := <-cb.res:
		if res.err != nil {
			res.err.Host = h
			return "", res.err
		}
		if res.miss {
			return "", fmt.Errorf("oauth: login at %s: callback carried no authorization code", h)
		}
		return res.code, nil
	case <-ctx.Done():
		return "", fmt.Errorf("oauth: login at %s: waiting for the browser callback: %w", h, ctx.Err())
	}
}

// LoginU2M runs the browser authorization-code + PKCE flow against host and returns access and refresh tokens.
func LoginU2M(ctx context.Context, host string, opts LoginOptions) (Token, error) {
	h, err := normHost(host)
	if err != nil {
		return Token{}, err
	}
	if opts.Port == 0 {
		opts.Port = defaultPort
	}
	if opts.Timeout == 0 {
		opts.Timeout = loginTimeout
	}
	if opts.Out == nil {
		opts.Out = os.Stderr
	}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	verifier, err1 := randomB64(32)
	state, err2 := randomB64(16)
	if err := errors.Join(err1, err2); err != nil {
		return Token{}, fmt.Errorf("oauth: login at %s: %w", h, err)
	}
	ln, port, err := listenLoopback(opts.Port)
	if err != nil {
		return Token{}, err
	}
	cb := &callback{state: state, res: make(chan callbackResult, 1)}
	srv := &http.Server{Handler: cb, ReadHeaderTimeout: 10 * time.Second}
	for _, l := range ln.ls {
		go func() { _ = srv.Serve(l) }()
	}
	defer srv.Close()

	redirect := "http://localhost:" + strconv.Itoa(port)
	authURL := authorizeURL(h, redirect, state, verifier)
	if opts.Open == nil || opts.Open(authURL) != nil {
		fmt.Fprintf(opts.Out, "Open this URL in a browser to sign in to %s:\n%s\n", h, authURL)
	}
	code, err := await(ctx, h, cb)
	if err != nil {
		return Token{}, err
	}
	reply, err := postToken(ctx, tokenRequest{host: h, op: "login", form: url.Values{
		"grant_type": {"authorization_code"}, "client_id": {ClientID}, "code": {code},
		"code_verifier": {verifier}, "redirect_uri": {redirect},
	}})
	if err != nil {
		return Token{}, err
	}
	if reply.RefreshToken == "" {
		return Token{}, fmt.Errorf("oauth: login at %s: no refresh token issued (offline_access not granted)", h)
	}
	return reply.token(reply.RefreshToken), nil
}
