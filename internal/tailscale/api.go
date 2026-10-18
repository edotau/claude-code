// Package tailscale talks to the Tailscale admin API (net/http JSON) and execs the installed
// `tailscale` CLI. Stdlib only — no tailscale.com import anywhere in this package.
package tailscale

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/secrets"
)

// DefaultBaseURL is the Tailscale admin API.
const DefaultBaseURL = "https://api.tailscale.com/api/v2"

// ErrNoCredential: neither an OAuth client nor an API key resolves via the secrets ladder.
var ErrNoCredential = errors.New("tailscale: no credential (set TAILSCALE_OAUTH_CLIENT_ID/_SECRET or TAILSCALE_API_KEY)")

// Client talks to the admin API; token mints (and caches) the bearer, re-minted once on a 401.
type Client struct {
	BaseURL string
	Tailnet string
	HTTP    *http.Client
	token   func(ctx context.Context) (string, error)
}

// NewClient resolves credentials via the same ladder providers use (env -> keyring -> secrets.env):
// TAILSCALE_OAUTH_CLIENT_ID/_SECRET preferred, else TAILSCALE_API_KEY as a bearer directly.
func NewClient() (*Client, error) {
	c := &Client{BaseURL: DefaultBaseURL, Tailnet: "-", HTTP: http.DefaultClient}
	if id, _, ok := secrets.Get("TAILSCALE_OAUTH_CLIENT_ID"); ok {
		if secret, _, ok2 := secrets.Get("TAILSCALE_OAUTH_CLIENT_SECRET"); ok2 {
			c.token = oauthToken(c.BaseURL, id, secret, c.HTTP)
			return c, nil
		}
	}
	if key, _, ok := secrets.Get("TAILSCALE_API_KEY"); ok {
		c.token = func(context.Context) (string, error) { return key, nil }
		return c, nil
	}
	return nil, ErrNoCredential
}

// request sends an authenticated JSON request, decodes a 2xx body into out, and re-mints the token
// once on a 401 before giving up.
func (c *Client) request(ctx context.Context, method, path string, body, out any) error {
	resp, err := c.send(ctx, method, path, body, false)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		resp, err = c.send(ctx, method, path, body, true)
		if err != nil {
			return err
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("tailscale: %s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) send(ctx context.Context, method, path string, body any, forceRefresh bool) (*http.Response, error) {
	if forceRefresh {
		ctx = withForceRefresh(ctx)
	}
	tok, err := c.token(ctx)
	if err != nil {
		return nil, err
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.HTTP.Do(req)
}

// KeyOpts configures a minted auth key.
type KeyOpts struct {
	Ephemeral     bool
	Preauthorized bool
	Reusable      bool
	Tags          []string
	Expiry        time.Duration
	Description   string
}

// AuthKey is a minted key; Key is a secret — callers must never log or audit its value.
type AuthKey struct {
	ID      string    `json:"id"`
	Key     string    `json:"key"`
	Expires time.Time `json:"expires"`
}

// CreateAuthKey mints a tskey-auth-… key via POST /tailnet/{tailnet}/keys.
func (c *Client) CreateAuthKey(ctx context.Context, opts KeyOpts) (AuthKey, error) {
	type create struct {
		Reusable      bool     `json:"reusable"`
		Ephemeral     bool     `json:"ephemeral"`
		Preauthorized bool     `json:"preauthorized"`
		Tags          []string `json:"tags"`
	}
	body := struct {
		Capabilities struct {
			Devices struct {
				Create create `json:"create"`
			} `json:"devices"`
		} `json:"capabilities"`
		ExpirySeconds int    `json:"expirySeconds,omitempty"`
		Description   string `json:"description,omitempty"`
	}{}
	body.Capabilities.Devices.Create = create{
		Reusable: opts.Reusable, Ephemeral: opts.Ephemeral, Preauthorized: opts.Preauthorized, Tags: opts.Tags,
	}
	if opts.Expiry > 0 {
		body.ExpirySeconds = int(opts.Expiry.Seconds())
	}
	body.Description = opts.Description

	var out AuthKey
	if err := c.request(ctx, http.MethodPost, "/tailnet/"+c.Tailnet+"/keys", body, &out); err != nil {
		return AuthKey{}, err
	}
	return out, nil
}

// Device is one tailnet device as the admin API reports it.
type Device struct {
	ID               string    `json:"id"`
	NodeID           string    `json:"nodeId"`
	Name             string    `json:"name"`
	Hostname         string    `json:"hostname"`
	OS               string    `json:"os"`
	Addresses        []string  `json:"addresses"`
	Tags             []string  `json:"tags"`
	LastSeen         time.Time `json:"lastSeen"`
	Authorized       bool      `json:"authorized"`
	AdvertisedRoutes []string  `json:"advertisedRoutes,omitempty"`
	EnabledRoutes    []string  `json:"enabledRoutes,omitempty"`
}

// Devices lists every device on the tailnet via GET /tailnet/{tailnet}/devices.
func (c *Client) Devices(ctx context.Context) ([]Device, error) {
	var out struct {
		Devices []Device `json:"devices"`
	}
	if err := c.request(ctx, http.MethodGet, "/tailnet/"+c.Tailnet+"/devices", nil, &out); err != nil {
		return nil, err
	}
	return out.Devices, nil
}

// ApproveExitNode approves device id's default-route advertisement via POST /device/{id}/routes.
func (c *Client) ApproveExitNode(ctx context.Context, deviceID string) error {
	body := struct {
		Routes []string `json:"routes"`
	}{Routes: []string{"0.0.0.0/0", "::/0"}}
	return c.request(ctx, http.MethodPost, "/device/"+deviceID+"/routes", body, nil)
}
