// Package dbxoauth mints Databricks workspace access tokens natively (stdlib only): U2M browser PKCE + refresh, M2M.
package dbxoauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
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
func normHost(host string) (string, error) {
	h := strings.TrimRight(strings.TrimSpace(host), "/")
	u, err := url.Parse(h)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", errors.New("dbxoauth: host must be an https URL such as https://example.cloud.databricks.com")
	}
	if hn := u.Hostname(); u.Scheme == "http" && hn != "localhost" && hn != "127.0.0.1" && hn != "::1" {
		return "", fmt.Errorf("dbxoauth: host %s uses scheme http; credentials need https (loopback excepted)", u.Host)
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
	s := fmt.Sprintf("dbxoauth: %s at %s failed (HTTP %d)", e.Op, e.Host, e.Status)
	if e.Code != "" {
		s += ": " + e.Code
	}
	if e.Desc != "" {
		s += ": " + e.Desc
	}
	return s
}

// IsInvalidGrant reports a rejected refresh token or credential (the caller must re-login or fix the secret).
func IsInvalidGrant(err error) bool {
	var oe *oauthError
	return errors.As(err, &oe) && (oe.Code == "invalid_grant" || oe.Code == "invalid_client" || oe.Code == "unauthorized_client")
}

// postToken POSTs a form to the host's token endpoint, retrying transient failures (transport, 429, 5xx) twice.
func postToken(ctx context.Context, host, op string, form url.Values, basicID, basicSecret string) (Token, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return Token{}, fmt.Errorf("dbxoauth: %s at %s: %w", op, host, ctx.Err())
			case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
			}
		}
		tok, transient, err := postTokenOnce(ctx, host, op, form, basicID, basicSecret)
		if err == nil {
			return tok, nil
		}
		lastErr = err
		if !transient {
			break
		}
	}
	return Token{}, lastErr
}

func postTokenOnce(ctx context.Context, host, op string, form url.Values, basicID, basicSecret string) (Token, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL(host), strings.NewReader(form.Encode()))
	if err != nil {
		return Token{}, false, fmt.Errorf("dbxoauth: %s at %s: bad request", op, host)
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
		var ue *url.Error
		if errors.As(err, &ue) {
			cause = ue.Err
		}
		return Token{}, ctx.Err() == nil, fmt.Errorf("dbxoauth: %s at %s: network error: %w", op, host, cause)
	}
	defer resp.Body.Close()
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if rerr != nil {
		return Token{}, true, fmt.Errorf("dbxoauth: %s at %s: reading response: %w", op, host, rerr)
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
		transient := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		return Token{}, transient, &oauthError{Host: host, Op: op, Code: body.Error, Desc: body.ErrorDesc, Status: resp.StatusCode}
	}
	if jerr != nil || body.AccessToken == "" {
		return Token{}, false, fmt.Errorf("dbxoauth: %s at %s: response had no access_token", op, host)
	}
	tok := Token{Access: body.AccessToken, Refresh: body.RefreshToken}
	if body.ExpiresIn > 0 {
		tok.Expiry = now().Add(time.Duration(body.ExpiresIn * float64(time.Second)))
	}
	return tok, false, nil
}

// RefreshU2M exchanges a refresh token for a new access token; the returned Refresh may be rotated.
func RefreshU2M(ctx context.Context, host, refreshToken string) (Token, error) {
	h, err := normHost(host)
	if err != nil {
		return Token{}, err
	}
	if refreshToken == "" {
		return Token{}, fmt.Errorf("dbxoauth: refresh at %s: empty refresh token", h)
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {ClientID},
		"refresh_token": {refreshToken},
	}
	tok, err := postToken(ctx, h, "refresh", form, "", "")
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
		return Token{}, fmt.Errorf("dbxoauth: m2m at %s: client id and secret are required", h)
	}
	l := hostLock(h)
	l.Lock()
	defer l.Unlock()
	form := url.Values{"grant_type": {"client_credentials"}, "scope": {scopeM2M}}
	tok, err := postToken(ctx, h, "client-credentials grant", form, clientID, clientSecret)
	if err != nil {
		return Token{}, err
	}
	tok.Refresh = ""
	return tok, nil
}
