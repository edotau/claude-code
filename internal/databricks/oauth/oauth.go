// Package oauth mints Databricks access tokens natively (stdlib only): U2M browser PKCE + refresh, M2M.
package oauth

import (
	"bufio"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"

	"github.com/edotau/claude-code/internal/textutil"
)

// clientID is the public OAuth client id of the Databricks CLI (an identifier, not a secret).
const clientID = "databricks-cli"

// RetryBackoff is the token-endpoint retry step (attempt × step); a var so tests can shorten it.
var RetryBackoff = 500 * time.Millisecond

const (
	tokenPath    = "/oidc/v1/token"
	maxAttempts  = 3
	maxBodyBytes = 1 << 20
	hostHint     = "oauth: host must be an https URL such as https://example.cloud.databricks.com"
)

// Token is a minted access token; Refresh is empty for M2M.
type Token struct {
	Access  string
	Expiry  time.Time
	Refresh string
}

// oauthError is a token-endpoint rejection (RFC 6749 §5.2): the HTTP status plus the error code and description.
type oauthError struct {
	Host, Op, Code, Desc string
	Body                 string // a clipped non-JSON reply, for diagnostics
	Status               int
}

func (e *oauthError) Error() string {
	s := fmt.Sprintf("oauth: %s at %s failed (HTTP %d)", e.Op, e.Host, e.Status)
	if e.Code != "" {
		s += ": " + e.Code
		if e.Desc != "" {
			s += ": " + e.Desc
		}
	} else if e.Body != "" {
		s += ": " + e.Body
	}
	return s
}

// loginCodes are the RFC 6749 error codes a fresh browser login fixes.
var loginCodes = map[string]bool{
	"invalid_grant": true, "invalid_client": true, "unauthorized_client": true, "access_denied": true, "invalid_scope": true,
}

// IsLoginRequired reports a token-endpoint rejection a fresh browser login fixes; transport, 429 and 5xx are not.
func IsLoginRequired(err error) bool {
	var oe *oauthError
	return errors.As(err, &oe) && loginCodes[oe.Code]
}

// IsRejected reports a token endpoint that answered 401, or 403 with an OAuth error code (a bare 403 is a WAF/proxy).
func IsRejected(err error) bool {
	var oe *oauthError
	return errors.As(err, &oe) && (oe.Status == http.StatusUnauthorized || (oe.Status == http.StatusForbidden && oe.Code != ""))
}

// NormalizeHost is the canonical workspace host: bare host or path-less https URL in, lower-case https://host out.
func NormalizeHost(s string) string {
	s = strings.TrimRight(strings.TrimSpace(s), "/")
	if s == "" || strings.ContainsAny(s, "?#") {
		return ""
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" {
		return ""
	}
	return "https://" + strings.ToLower(u.Host)
}

// normHost is NormalizeHost for token calls, which also take loopback http and a path (test servers); no bare host.
func normHost(host string) (string, error) {
	host = strings.TrimRight(strings.TrimSpace(host), "/")
	u, err := url.Parse(host)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", errors.New(hostHint)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(host, "?#") {
		return "", errors.New(hostHint)
	}
	if u.Scheme == "http" && !textutil.IsLoopbackHost(u.Hostname()) {
		return "", fmt.Errorf("oauth: host %s uses scheme http; credentials need https (loopback excepted)", u.Hostname())
	}
	return cmp.Or(NormalizeHost(host), host), nil
}

// tokenReply is the success body of the token endpoint.
type tokenReply struct {
	AccessToken  string  `json:"access_token"`
	RefreshToken string  `json:"refresh_token"`
	ExpiresIn    float64 `json:"expires_in"`
}

type tokenRequest struct {
	host, op string
	form     url.Values
	user     string // basic-auth client id, empty for none
	secret   string
	replay   bool // the grant is idempotent: a request that may have been processed can be sent again
}

var tokenClient = &http.Client{Timeout: 30 * time.Second}

// retryable reports whether a failed attempt may be sent again; a one-shot grant retries only 429/503
// or a transport error before the request was written (it cannot have been consumed).
func (r tokenRequest) retryable(status int, written bool) bool {
	if status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable {
		return true
	}
	if status == 0 && !written {
		return true
	}
	return r.replay && (status == 0 || status >= 500)
}

type noRetryKey struct{}

// WithoutRetry makes token requests on ctx single-shot: a probe reports a 429 or 503, it never waits one out.
func WithoutRetry(ctx context.Context) context.Context {
	return context.WithValue(ctx, noRetryKey{}, true)
}

func noRetry(ctx context.Context) bool { v, _ := ctx.Value(noRetryKey{}).(bool); return v }

// postToken sends the grant, retrying transient failures up to maxAttempts with an attempt × RetryBackoff pause.
func postToken(ctx context.Context, r tokenRequest) (tokenReply, error) {
	var (
		reply  tokenReply
		status int
		sent   bool
		err    error
	)
	for attempt := 1; ; attempt++ {
		reply, status, sent, err = postOnce(ctx, r)
		if err == nil || !r.retryable(status, sent) || attempt == maxAttempts || noRetry(ctx) {
			return reply, err
		}
		select {
		case <-ctx.Done():
			return reply, err
		case <-time.After(time.Duration(attempt) * RetryBackoff):
		}
	}
}

// postOnce is one attempt; status is 0 with no HTTP response, written is true once the request was sent.
func postOnce(ctx context.Context, r tokenRequest) (reply tokenReply, status int, written bool, err error) {
	var conn, writeFailed atomic.Bool // set by transport goroutines, possibly after Do has returned
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn:      func(httptrace.GotConnInfo) { conn.Store(true) },
		WroteRequest: func(info httptrace.WroteRequestInfo) { writeFailed.Store(info.Err != nil) },
	})
	reply, status, err = doPost(ctx, r)
	// A connection was obtained and no write failure was reported: assume sent, since the hook may fire late.
	return reply, status, conn.Load() && !writeFailed.Load(), err
}

// doPost sends the token request on ctx and decodes the reply.
func doPost(ctx context.Context, r tokenRequest) (reply tokenReply, status int, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.host+tokenPath, strings.NewReader(r.form.Encode()))
	if err != nil {
		return reply, 0, fmt.Errorf("oauth: %s at %s: %w", r.op, r.host, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if r.user != "" {
		req.SetBasicAuth(r.user, r.secret)
	}
	resp, err := tokenClient.Do(req)
	if err != nil {
		return reply, 0, fmt.Errorf("oauth: %s at %s: network error: %w", r.op, r.host, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return reply, 0, fmt.Errorf("oauth: %s at %s: network error: %w", r.op, r.host, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e struct {
			Error       string `json:"error"`
			Description string `json:"error_description"`
		}
		oe := &oauthError{Host: r.host, Op: r.op, Status: resp.StatusCode}
		if json.Unmarshal(raw, &e) == nil {
			oe.Code, oe.Desc = e.Error, e.Description
		} else {
			oe.Body = clipBody(raw)
		}
		return reply, resp.StatusCode, oe
	}
	if json.Unmarshal(raw, &reply) != nil || reply.AccessToken == "" {
		return reply, resp.StatusCode, fmt.Errorf("oauth: %s at %s: response had no access_token", r.op, r.host)
	}
	return reply, resp.StatusCode, nil
}

// clipBody is the first 120 runes of a non-JSON error body on one line, control bytes replaced by spaces.
func clipBody(raw []byte) string {
	clean := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(string(raw), "?"))
	return textutil.CapRunes(strings.Join(strings.Fields(clean), " "), 120, "…")
}

// token converts a reply into a Token whose Expiry is zero when the server sent no lifetime.
func (t tokenReply) token(refresh string) Token {
	tok := Token{Access: t.AccessToken, Refresh: refresh}
	if t.ExpiresIn > 0 {
		tok.Expiry = time.Now().Add(time.Duration(t.ExpiresIn * float64(time.Second)))
	}
	return tok
}

// RefreshU2M exchanges a refresh token for a new access token; the returned Refresh may be rotated.
func RefreshU2M(ctx context.Context, host, refreshToken string) (Token, error) {
	h, err := normHost(host)
	if err != nil {
		return Token{}, err
	}
	if refreshToken == "" {
		return Token{}, fmt.Errorf("oauth: refresh at %s: empty refresh token", h)
	}
	reply, err := postToken(ctx, tokenRequest{host: h, op: "refresh", form: url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {refreshToken}, "client_id": {clientID},
	}})
	if err != nil {
		return Token{}, err
	}
	if reply.RefreshToken != "" {
		refreshToken = reply.RefreshToken
	}
	return reply.token(refreshToken), nil
}

var m2mLocks sync.Map // host+clientID -> chan struct{} (a 1-slot semaphore, so the wait can honor ctx)

// MintM2M runs the service-principal client-credentials grant (scope all-apis); Refresh is always empty.
func MintM2M(ctx context.Context, host, clientID, clientSecret string) (Token, error) {
	h, err := normHost(host)
	if err != nil {
		return Token{}, err
	}
	if clientID == "" || clientSecret == "" {
		return Token{}, fmt.Errorf("oauth: m2m at %s: client id and secret are required", h)
	}
	sem, _ := m2mLocks.LoadOrStore(h+"\x00"+clientID, make(chan struct{}, 1))
	select {
	case sem.(chan struct{}) <- struct{}{}:
		defer func() { <-sem.(chan struct{}) }()
	case <-ctx.Done():
		return Token{}, fmt.Errorf("oauth: m2m at %s: waiting for a concurrent mint: %w", h, ctx.Err())
	}
	reply, err := postToken(ctx, tokenRequest{
		host: h, op: "client-credentials grant", user: clientID, secret: clientSecret, replay: true,
		form: url.Values{"grant_type": {"client_credentials"}, "scope": {"all-apis"}},
	})
	if err != nil {
		return Token{}, err
	}
	return reply.token(""), nil
}

const (
	authorizePath = "/oidc/v1/authorize"
	DefaultPort   = 8030 // first callback port tried; a --host login forwards it
	PortSpan      = 20   // ports tried above DefaultPort when it is busy
	loginTimeout  = 5 * time.Minute
	failedPage    = "Login failed. You can close this tab."
)

// LoginOptions tunes LoginU2M; the zero value is the interactive default.
type LoginOptions struct {
	// Open launches the browser; its error is reported after the URL, which LoginU2M always prints to Out.
	Open func(authURL string) error
	// Out receives the sign-in URL and the SSH hint (default os.Stderr).
	Out io.Writer
	// Timeout bounds the whole login (default 5 minutes).
	Timeout time.Duration
	// Port is the first localhost callback port to try (default 8030, falling back upward to 8050).
	Port int
	// ExactPort tries Port alone: a busy port is an error, never a walk upward.
	ExactPort bool
	// NoBrowser never calls Open and always prints the SSH forward hint.
	NoBrowser bool
	// Paste, when set, also reads one pasted redirect URL from it; whichever of that and the callback comes first wins.
	Paste io.Reader
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

// listenPorts binds 127.0.0.1 (and ::1 when the host has it) on the first free port from first to last; first == last
// is an exact port, and busy is an error naming it.
func listenPorts(first, last int) (*loopback, int, error) {
	var lc net.ListenConfig
	var other error // the last listen error that was not EADDRINUSE
	for port := first; port <= last; port++ {
		v4, err := lc.Listen(context.Background(), "tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			if !errors.Is(err, syscall.EADDRINUSE) {
				other = err
			}
			continue
		}
		lb := &loopback{ls: []net.Listener{v4}}
		v6, err := lc.Listen(context.Background(), "tcp6", net.JoinHostPort("::1", strconv.Itoa(port)))
		switch {
		case err == nil:
			lb.ls = append(lb.ls, v6)
		case errors.Is(err, syscall.EADDRINUSE):
			_ = v4.Close()
			continue
		}
		return lb, port, nil
	}
	if first == last {
		if other != nil {
			return nil, 0, fmt.Errorf("oauth: login callback port %d on localhost: %w", first, other)
		}
		return nil, 0, fmt.Errorf("oauth: login callback port %d on localhost is busy: another process holds it", first)
	}
	if other != nil {
		return nil, 0, fmt.Errorf("oauth: login callback ports %d-%d on localhost: %w", first, last, other)
	}
	return nil, 0, fmt.Errorf("oauth: login callback ports %d-%d on localhost are busy (another login running?)", first, last)
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
		"client_id": {clientID}, "response_type": {"code"}, "redirect_uri": {redirect}, "scope": {"all-apis offline_access"},
		"state": {state}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"},
	}.Encode()
}

// readPaste reads one line from r in the background; a closed stdin with no data sends nothing.
func readPaste(r io.Reader) <-chan string {
	ch := make(chan string, 1)
	go func() {
		if line, err := bufio.NewReader(r).ReadString('\n'); err == nil || line != "" {
			ch <- line
		}
	}()
	return ch
}

// parsePaste checks the state of a pasted redirect URL and classifies it; the text is never echoed in an error.
func parsePaste(line, state string) (callbackResult, error) {
	line = strings.TrimSpace(line)
	if line == "" {
		return callbackResult{}, errors.New("nothing was pasted; expected the full redirected URL")
	}
	u, err := url.Parse(line)
	if err != nil || u.RawQuery == "" {
		return callbackResult{}, errors.New("the pasted text is not a redirected URL carrying code and state")
	}
	q := u.Query()
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 {
		return callbackResult{}, errors.New("the pasted URL has the wrong state; paste the redirect of this login attempt")
	}
	return classify(q), nil
}

// await blocks for the browser redirect or a pasted one (paste may be nil) and returns its authorization code.
func await(ctx context.Context, h string, cb *callback, paste <-chan string) (string, error) {
	select {
	case res := <-cb.res:
		return codeOf(h, res)
	case line := <-paste:
		res, err := parsePaste(line, cb.state)
		if err != nil {
			return "", fmt.Errorf("oauth: login at %s: %w", h, err)
		}
		return codeOf(h, res)
	case <-ctx.Done():
		return "", fmt.Errorf("oauth: login at %s: waiting for the browser callback: %w", h, ctx.Err())
	}
}

// codeOf returns the authorization code of a redirect result, or its error.
func codeOf(h string, res callbackResult) (string, error) {
	if res.err != nil {
		res.err.Host = h
		return "", res.err
	}
	if res.miss {
		return "", fmt.Errorf("oauth: login at %s: callback carried no authorization code", h)
	}
	return res.code, nil
}

// LoginU2M runs the browser authorization-code + PKCE flow against host and returns access and refresh tokens.
func LoginU2M(ctx context.Context, host string, opts LoginOptions) (Token, error) {
	h, err := normHost(host)
	if err != nil {
		return Token{}, err
	}
	opts = opts.withDefaults()
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	verifier, err1 := randomB64(32)
	state, err2 := randomB64(16)
	if err := errors.Join(err1, err2); err != nil {
		return Token{}, fmt.Errorf("oauth: login at %s: %w", h, err)
	}
	last := opts.Port + PortSpan
	if opts.ExactPort {
		last = opts.Port
	}
	ln, port, err := listenPorts(opts.Port, last)
	if err != nil {
		return Token{}, err
	}
	cb := &callback{state: state, res: make(chan callbackResult, 1)}
	srv := serveCallback(ln, cb)
	defer stopCallback(srv)

	redirect := "http://localhost:" + strconv.Itoa(port)
	authURL := authorizeURL(h, redirect, state, verifier)
	var openErr error
	if opts.Open != nil && !opts.NoBrowser {
		openErr = opts.Open(authURL)
	}
	fmt.Fprintf(opts.Out, "Open this URL in a browser to sign in to %s:\n%s\n", h, authURL)
	if openErr != nil {
		fmt.Fprintf(opts.Out, "No browser opened here (%v); open the URL above yourself.\n", openErr)
	}
	fmt.Fprint(opts.Out, forwardHint(port, opts.NoBrowser || openErr != nil))
	var paste <-chan string
	if opts.Paste != nil {
		fmt.Fprintf(opts.Out, "No forward? Paste the full redirected URL (http://localhost:%d/?code=...&state=...) and press Enter:\n", port)
		paste = readPaste(opts.Paste)
	}
	code, err := await(ctx, h, cb, paste)
	if err != nil {
		return Token{}, err
	}
	return exchangeCode(ctx, h, code, verifier, redirect)
}

// withDefaults fills the zero port, timeout and output of opts.
func (opts LoginOptions) withDefaults() LoginOptions {
	if opts.Port == 0 {
		opts.Port = DefaultPort
	}
	if opts.Timeout == 0 {
		opts.Timeout = loginTimeout
	}
	if opts.Out == nil {
		opts.Out = os.Stderr
	}
	return opts
}

// stopCallback lets an in-flight "Login complete" response finish before closing; Close alone cut it off (EOF).
func stopCallback(srv *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if srv.Shutdown(ctx) != nil {
		_ = srv.Close()
	}
}

// serveCallback starts the loopback server on every listener of ln.
func serveCallback(ln *loopback, cb *callback) *http.Server {
	srv := &http.Server{Handler: cb, ReadHeaderTimeout: 10 * time.Second}
	for _, l := range ln.ls {
		go func() { _ = srv.Serve(l) }()
	}
	return srv
}

// exchangeCode trades the authorization code for tokens; a reply without a refresh token is an error.
func exchangeCode(ctx context.Context, h, code, verifier, redirect string) (Token, error) {
	reply, err := postToken(ctx, tokenRequest{host: h, op: "login", form: url.Values{
		"grant_type": {"authorization_code"}, "client_id": {clientID}, "code": {code},
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

// ViaHostEnv carries the --host name into a remote login: the caller already forwards the port or opens the browser here.
const ViaHostEnv = "HARNESS_LOGIN_VIA_HOST"

// forwardHint is the line after the URL: where the browser opens under --host, else the ssh -L command naming this host.
func forwardHint(port int, noBrowser bool) string {
	via := os.Getenv(ViaHostEnv)
	switch {
	case via != "" && noBrowser:
		return "" // the --host caller printed its own forwarding line
	case via != "":
		return fmt.Sprintf("Opening the browser on %s; the page can take a second.\n", via)
	case !noBrowser && os.Getenv("SSH_CONNECTION") == "":
		return ""
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "<this-host>"
	}
	return fmt.Sprintf("Forward the callback first: ssh -L %d:localhost:%d %s\n", port, port, host)
}
