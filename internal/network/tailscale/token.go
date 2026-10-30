package tailscale

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// forceRefreshKey marks a context so oauthToken re-mints even if its cache looks fresh (used once
// after a 401, since the API's own clock may disagree with ours).
type forceRefreshKey struct{}

func withForceRefresh(ctx context.Context) context.Context {
	return context.WithValue(ctx, forceRefreshKey{}, true)
}

// oauthToken returns a token func that mints an access token via OAuth client_credentials
// (POST /oauth/token, form-encoded client_id/client_secret) and caches it in-process until
// expires_in-60s.
func oauthToken(baseURL, clientID, clientSecret string, httpClient *http.Client) func(ctx context.Context) (string, error) {
	var (
		mu      sync.Mutex
		cached  string
		expires time.Time
	)
	return func(ctx context.Context) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		force, _ := ctx.Value(forceRefreshKey{}).(bool)
		if !force && cached != "" && time.Now().Before(expires) {
			return cached, nil
		}
		form := url.Values{"client_id": {clientID}, "client_secret": {clientSecret}}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/oauth/token", strings.NewReader(form.Encode()))
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := httpClient.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("tailscale: oauth token mint: HTTP %d", resp.StatusCode)
		}
		var out struct {
			AccessToken string `json:"access_token"`
			ExpiresIn   int    `json:"expires_in"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return "", err
		}
		if out.AccessToken == "" {
			return "", errors.New("tailscale: oauth token mint: empty access_token")
		}
		cached = out.AccessToken
		expires = time.Now().Add(time.Duration(out.ExpiresIn)*time.Second - 60*time.Second)
		return cached, nil
	}
}
