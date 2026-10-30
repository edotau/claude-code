// Package oauth mints Databricks access tokens natively (stdlib only): U2M browser PKCE + refresh, M2M.
package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ClientID is the public OAuth client id of the Databricks CLI (an identifier, not a secret).
const ClientID = "databricks-cli"

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
	Status               int
}

func (e *oauthError) Error() string {
	s := fmt.Sprintf("oauth: %s at %s failed (HTTP %d)", e.Op, e.Host, e.Status)
	if e.Code != "" {
		s += ": " + e.Code
		if e.Desc != "" {
			s += ": " + e.Desc
		}
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

// IsRejected reports a token endpoint that answered 401 or 403 (credentials refused, not a transient failure).
func IsRejected(err error) bool {
	var oe *oauthError
	return errors.As(err, &oe) && (oe.Status == http.StatusUnauthorized || oe.Status == http.StatusForbidden)
}

// normHost trims host to scheme://authority[/path]; plain http is accepted only for loopback hosts.
func normHost(host string) (string, error) {
	host = strings.TrimRight(strings.TrimSpace(host), "/")
	u, err := url.Parse(host)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", errors.New(hostHint)
	}
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		return "", fmt.Errorf("oauth: host %s uses scheme http; credentials need https (loopback excepted)", u.Hostname())
	}
	return host, nil
}

func isLoopbackHost(h string) bool {
	ip := net.ParseIP(h)
	return h == "localhost" || (ip != nil && ip.IsLoopback())
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
		if err == nil || !r.retryable(status, sent) || attempt == maxAttempts {
			return reply, err
		}
		select {
		case <-ctx.Done():
			return reply, err
		case <-time.After(time.Duration(attempt) * RetryBackoff):
		}
	}
}

// postOnce is one attempt; status is 0 when no HTTP response arrived and written is true once the request went out.
func postOnce(ctx context.Context, r tokenRequest) (reply tokenReply, status int, written bool, err error) {
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		WroteRequest: func(info httptrace.WroteRequestInfo) { written = info.Err == nil },
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.host+tokenPath, strings.NewReader(r.form.Encode()))
	if err != nil {
		return reply, 0, written, fmt.Errorf("oauth: %s at %s: %w", r.op, r.host, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if r.user != "" {
		req.SetBasicAuth(r.user, r.secret)
	}
	resp, err := tokenClient.Do(req)
	if err != nil {
		return reply, 0, written, fmt.Errorf("oauth: %s at %s: network error: %w", r.op, r.host, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return reply, 0, written, fmt.Errorf("oauth: %s at %s: network error: %w", r.op, r.host, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e struct {
			Error       string `json:"error"`
			Description string `json:"error_description"`
		}
		_ = json.Unmarshal(raw, &e)
		return reply, resp.StatusCode, written, &oauthError{Host: r.host, Op: r.op, Code: e.Error, Desc: e.Description, Status: resp.StatusCode}
	}
	if json.Unmarshal(raw, &reply) != nil || reply.AccessToken == "" {
		return reply, resp.StatusCode, written, fmt.Errorf("oauth: %s at %s: response had no access_token", r.op, r.host)
	}
	return reply, resp.StatusCode, written, nil
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
		"grant_type": {"refresh_token"}, "refresh_token": {refreshToken}, "client_id": {ClientID},
	}})
	if err != nil {
		return Token{}, err
	}
	if reply.RefreshToken != "" {
		refreshToken = reply.RefreshToken
	}
	return reply.token(refreshToken), nil
}

var m2mLocks sync.Map // host -> *sync.Mutex

// MintM2M runs the service-principal client-credentials grant (scope all-apis); Refresh is always empty.
func MintM2M(ctx context.Context, host, clientID, clientSecret string) (Token, error) {
	h, err := normHost(host)
	if err != nil {
		return Token{}, err
	}
	if clientID == "" || clientSecret == "" {
		return Token{}, fmt.Errorf("oauth: m2m at %s: client id and secret are required", h)
	}
	mu, _ := m2mLocks.LoadOrStore(h, new(sync.Mutex))
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()
	reply, err := postToken(ctx, tokenRequest{
		host: h, op: "client-credentials grant", user: clientID, secret: clientSecret, replay: true,
		form: url.Values{"grant_type": {"client_credentials"}, "scope": {"all-apis"}},
	})
	if err != nil {
		return Token{}, err
	}
	return reply.token(""), nil
}
