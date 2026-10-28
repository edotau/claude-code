// Package sdkport mints Databricks access tokens natively (stdlib only): U2M browser PKCE + refresh, M2M.
package sdkport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// ClientID is the public OAuth client of the official Databricks CLI / Go SDK (credentials/u2m, appClientID).
	ClientID = "databricks-cli"

	scopeU2M = "all-apis offline_access"
	scopeM2M = "all-apis"

	requestTimeout = 30 * time.Second
	maxBody        = 1 << 20
)

// Token is a minted access token; Refresh is empty for M2M.
type Token struct {
	Access  string
	Expiry  time.Time
	Refresh string
}

var httpClient = &http.Client{Timeout: requestTimeout}

// now is replaceable in tests.
var now = time.Now

// normHost trims whitespace and a trailing slash and requires an https host (plain http only on loopback).

// RetryBackoff is the token-endpoint retry step (attempt × step); a var so tests can shorten it.
var RetryBackoff = 500 * time.Millisecond

func normHost(host string) (string, error) {
	h := strings.TrimRight(strings.TrimSpace(host), "/")
	u, err := url.Parse(h)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", errors.New("oauth: host must be an https URL such as https://example.cloud.databricks.com")
	}
	if hn := u.Hostname(); u.Scheme == "http" && hn != "localhost" && hn != "127.0.0.1" && hn != "::1" {
		return "", fmt.Errorf("oauth: host %s uses scheme http; credentials need https (loopback excepted)", u.Host)
	}
	return h, nil
}

func tokenURL(host string) string     { return host + "/oidc/v1/token" }
func authorizeURL(host string) string { return host + "/oidc/v1/authorize" }

// oauthError is a token-endpoint rejection; it carries only the host and the OAuth error fields.
type oauthError struct {
	Host, Op, Code, Desc string
	Status               int
}

func (e *oauthError) Error() string {
	s := fmt.Sprintf("oauth: %s at %s failed (HTTP %d)", e.Op, e.Host, e.Status)
	if e.Code != "" {
		s += ": " + e.Code
	}
	if e.Desc != "" {
		s += ": " + e.Desc
	}
	return s
}

// authRejectionCodes are the RFC 6749 §5.2 error codes (case-sensitive) that mean the grant itself is dead.
var authRejectionCodes = []string{"invalid_client", "unauthorized_client", "invalid_grant", "access_denied", "invalid_scope"}

// IsLoginRequired reports a token-endpoint rejection a fresh browser login fixes; transport, 429 and 5xx are not.
func IsLoginRequired(err error) bool {
	oe, ok := errors.AsType[*oauthError](err)
	return ok && slices.Contains(authRejectionCodes, oe.Code)
}

// retryPolicy says which token-endpoint failures a grant may repeat.
type retryPolicy int

const (
	// retryIdempotent (client credentials) repeats transport errors, 429 and 5xx.
	retryIdempotent retryPolicy = iota
	// retryOneShot (authorization code, refresh token) repeats only what the server cannot have processed.
	retryOneShot
)

// failure classifies one failed token request for the retry policy.
type failure struct {
	transport bool // no response arrived
	sent      bool // the request was fully written before the transport failure
	status    int  // HTTP status when a response arrived
}

// retryable: one-shot grants repeat a pre-send transport error, 429 and 503; idempotent ones also 5xx/any transport.
func (f failure) retryable(pol retryPolicy) bool {
	switch {
	case f.transport:
		return pol == retryIdempotent || !f.sent
	case f.status == http.StatusTooManyRequests || f.status == http.StatusServiceUnavailable:
		return true
	default:
		return pol == retryIdempotent && f.status >= 500
	}
}

// postToken POSTs a form to the host's token endpoint, retrying what the policy allows, twice.
func postToken(ctx context.Context, host, op string, form url.Values, basicID, basicSecret string, pol retryPolicy) (Token, error) {
	var lastErr error
	for attempt := range 3 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return Token{}, fmt.Errorf("oauth: %s at %s: %w", op, host, ctx.Err())
			case <-time.After(time.Duration(attempt) * RetryBackoff):
			}
		}
		tok, f, err := postTokenOnce(ctx, host, op, form, basicID, basicSecret)
		if err == nil {
			return tok, nil
		}
		lastErr = err
		if ctx.Err() != nil || !f.retryable(pol) {
			break
		}
	}
	return Token{}, lastErr
}

func postTokenOnce(ctx context.Context, host, op string, form url.Values, basicID, basicSecret string) (Token, failure, error) {
	var wrote atomic.Bool
	trace := &httptrace.ClientTrace{WroteRequest: func(httptrace.WroteRequestInfo) { wrote.Store(true) }}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodPost, tokenURL(host),
		strings.NewReader(form.Encode()))
	if err != nil {
		return Token{}, failure{}, fmt.Errorf("oauth: %s at %s: bad request", op, host)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if basicID != "" {
		req.SetBasicAuth(basicID, basicSecret)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		// url.Error embeds the URL only (no form body); keep just the cause.
		cause := err
		if ue, ok := errors.AsType[*url.Error](err); ok {
			cause = ue.Err
		}
		return Token{}, failure{transport: true, sent: wrote.Load()},
			fmt.Errorf("oauth: %s at %s: network error: %w", op, host, cause)
	}
	defer resp.Body.Close()
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if rerr != nil {
		return Token{}, failure{transport: true, sent: true}, fmt.Errorf("oauth: %s at %s: reading response: %w", op, host, rerr)
	}
	var body struct {
		AccessToken  string  `json:"access_token"`
		RefreshToken string  `json:"refresh_token"`
		ExpiresIn    float64 `json:"expires_in"`
		Error        string  `json:"error"`
		ErrorDesc    string  `json:"error_description"`
	}
	jerr := json.Unmarshal(raw, &body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Token{}, failure{status: resp.StatusCode},
			&oauthError{Host: host, Op: op, Code: body.Error, Desc: body.ErrorDesc, Status: resp.StatusCode}
	}
	if jerr != nil || body.AccessToken == "" {
		return Token{}, failure{status: resp.StatusCode}, fmt.Errorf("oauth: %s at %s: response had no access_token", op, host)
	}
	tok := Token{Access: body.AccessToken, Refresh: body.RefreshToken}
	if body.ExpiresIn > 0 {
		tok.Expiry = now().Add(time.Duration(body.ExpiresIn * float64(time.Second)))
	}
	return tok, failure{}, nil
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
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {ClientID},
		"refresh_token": {refreshToken},
	}
	tok, err := postToken(ctx, h, "refresh", form, "", "", retryOneShot)
	if err != nil {
		return Token{}, err
	}
	if tok.Refresh == "" {
		tok.Refresh = refreshToken // server did not rotate
	}
	return tok, nil
}

var (
	hostLocksMu sync.Mutex
	hostLocks   = map[string]*sync.Mutex{}
)

// hostLock serializes mints per host within the process (cross-process locking is the caller's).
func hostLock(host string) *sync.Mutex {
	hostLocksMu.Lock()
	defer hostLocksMu.Unlock()
	m, ok := hostLocks[host]
	if !ok {
		m = &sync.Mutex{}
		hostLocks[host] = m
	}
	return m
}

// MintM2M runs the service-principal client-credentials grant (scope all-apis); Refresh is always empty.
func MintM2M(ctx context.Context, host, clientID, clientSecret string) (Token, error) {
	h, err := normHost(host)
	if err != nil {
		return Token{}, err
	}
	if clientID == "" || clientSecret == "" {
		return Token{}, fmt.Errorf("oauth: m2m at %s: client id and secret are required", h)
	}
	l := hostLock(h)
	l.Lock()
	defer l.Unlock()
	form := url.Values{"grant_type": {"client_credentials"}, "scope": {scopeM2M}}
	tok, err := postToken(ctx, h, "client-credentials grant", form, clientID, clientSecret, retryIdempotent)
	if err != nil {
		return Token{}, err
	}
	tok.Refresh = ""
	return tok, nil
}

// IsRejected reports a token endpoint that answered 401 or 403 (credentials refused, not a transient failure).
func IsRejected(err error) bool {
	oe, ok := errors.AsType[*oauthError](err)
	return ok && (oe.Status == http.StatusUnauthorized || oe.Status == http.StatusForbidden)
}
