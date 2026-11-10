package tailscale

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// forceRefreshKey makes oauthToken re-mint despite a fresh cache (once after a 401: clocks may disagree).
type forceRefreshKey struct{}

func withForceRefresh(ctx context.Context) context.Context {
	return context.WithValue(ctx, forceRefreshKey{}, true)
}

// oauthToken mints an access token via OAuth client_credentials and caches it until expires_in-60s.
func oauthToken(
	baseURL, clientID, clientSecret string, httpClient *http.Client,
) func(ctx context.Context) (string, error) {
	var (
		sem     = make(chan struct{}, 1) // a channel, not a mutex, so a queued caller honors its ctx
		cached  string
		expires time.Time
	)
	return func(ctx context.Context) (string, error) {
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
		case <-ctx.Done():
			return "", ctx.Err()
		}
		force, _ := ctx.Value(forceRefreshKey{}).(bool)
		if !force && cached != "" && time.Now().Before(expires) {
			return cached, nil
		}
		out, err := mintToken(ctx, baseURL, clientID, clientSecret, httpClient)
		if err != nil {
			return "", err
		}
		cached = out.AccessToken
		expires = time.Now().Add(time.Duration(out.ExpiresIn)*time.Second - 60*time.Second)
		return cached, nil
	}
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
}

// mintToken POSTs the client_credentials form to /oauth/token and decodes the minted token.
func mintToken(
	ctx context.Context, baseURL, clientID, clientSecret string, httpClient *http.Client,
) (tokenResponse, error) {
	var out tokenResponse
	form := url.Values{"client_id": {clientID}, "client_secret": {clientSecret}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return out, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := httpClient.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("tailscale: oauth token mint: HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return out, err
	}
	if out.AccessToken == "" {
		return out, errors.New("tailscale: oauth token mint: empty access_token")
	}
	return out, nil
}
