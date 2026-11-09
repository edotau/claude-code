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
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/vault"
)

// defaultBaseURL is the Tailscale admin API.
const defaultBaseURL = "https://api.tailscale.com/api/v2"

// apiBaseURL is the base URL NewClient and Doctor's credential check mint tokens against;
// package-private so tests can point it at an httptest server.
var apiBaseURL = defaultBaseURL

// ErrNoCredential: neither an OAuth client nor an API key resolves via the secrets ladder.
var ErrNoCredential = errors.New("tailscale: no credential (set TAILSCALE_OAUTH_CLIENT_ID/_SECRET or TAILSCALE_API_KEY)")

// errNoDevice: no admin-API device carries any of this node's tailnet addresses.
var errNoDevice = errors.New("could not match this node's address to a device (is it connected?)")

// SetNames is what `vault set --tailscale` writes: the API key alone, or the OAuth id then secret.
func SetNames(apiKey bool) []string {
	if apiKey {
		return []string{"TAILSCALE_API_KEY"}
	}
	return []string{"TAILSCALE_OAUTH_CLIENT_ID", "TAILSCALE_OAUTH_CLIENT_SECRET"}
}

// maxKeyDays is the largest whole-day count a time.Duration holds (about 292 years).
const maxKeyDays = math.MaxInt64 / int64(24*time.Hour)

// ParseKeyExpiry accepts a Go duration or a bare "<N>d" day count.
func ParseKeyExpiry(s string) (time.Duration, error) {
	if n, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.Atoi(n)
		if err != nil {
			return 0, err
		}
		if int64(days) > maxKeyDays {
			return 0, fmt.Errorf("key expiry %q overflows: at most %dd", s, maxKeyDays)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

// selfDevice resolves this box to its admin-API device; ErrNoCredential means no client could be built.
func selfDevice(ctx context.Context) (*Client, Local, string, error) {
	client, err := NewClient()
	if err != nil {
		return nil, Local{}, "", err
	}
	st, err := Status(ctx)
	if err != nil { // ErrNotRunning included: st.IPs would be stale and never match a device
		return nil, Local{}, "", err
	}
	devices, err := client.Devices(ctx)
	if err != nil {
		return nil, Local{}, "", err
	}
	for _, d := range devices {
		if slices.ContainsFunc(d.Addresses, func(a string) bool { return slices.Contains(st.IPs, a) }) {
			return client, st, d.ID, nil
		}
	}
	return nil, Local{}, "", errNoDevice
}

// Client talks to the admin API; token mints (and caches) the bearer, re-minted once on a 401.
type Client struct {
	BaseURL string
	Tailnet string
	HTTP    *http.Client
	token   func(ctx context.Context) (string, error)
}

// apiTimeout bounds one admin-API round trip so a stalled server cannot hang a verb.
const apiTimeout = 30 * time.Second

// NewClient resolves credentials via vault.Get (the vault, then env):
// TAILSCALE_OAUTH_CLIENT_ID/_SECRET preferred, else TAILSCALE_API_KEY as a bearer directly.
func NewClient() (*Client, error) {
	// DefaultClient's transport stays the seam tests swap; only the timeout is added.
	c := &Client{BaseURL: apiBaseURL, Tailnet: "-", HTTP: &http.Client{Transport: http.DefaultClient.Transport, Timeout: apiTimeout}}
	if id, _, ok := vault.Get("TAILSCALE_OAUTH_CLIENT_ID"); ok {
		if secret, _, ok2 := vault.Get("TAILSCALE_OAUTH_CLIENT_SECRET"); ok2 {
			c.token = oauthToken(c.BaseURL, id, secret, c.HTTP)
			return c, nil
		}
	}
	if key, _, ok := vault.Get("TAILSCALE_API_KEY"); ok {
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
		_ = resp.Body.Close()
		resp, err = c.send(ctx, method, path, body, true)
		if err != nil {
			return err
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
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

// deviceRoutes is GET /device/{id}/routes: what the device advertises and what the tailnet has enabled.
type deviceRoutes struct {
	Advertised []string `json:"advertisedRoutes"`
	Enabled    []string `json:"enabledRoutes"`
}

// Routes reads device id's advertised and enabled routes.
func (c *Client) Routes(ctx context.Context, deviceID string) (deviceRoutes, error) {
	var out deviceRoutes
	err := c.request(ctx, http.MethodGet, "/device/"+deviceID+"/routes", nil, &out)
	return out, err
}

// EnableRoutes adds routes to cur.Enabled and POSTs the union (the endpoint replaces the set); returns the new enabled set.
func (c *Client) EnableRoutes(ctx context.Context, deviceID string, cur deviceRoutes, routes []string) ([]string, error) {
	want := slices.Clone(cur.Enabled)
	for _, r := range routes {
		if !slices.Contains(want, r) {
			want = append(want, r)
		}
	}
	body := struct {
		Routes []string `json:"routes"`
	}{Routes: want}
	var out deviceRoutes
	if err := c.request(ctx, http.MethodPost, "/device/"+deviceID+"/routes", body, &out); err != nil {
		return nil, err
	}
	return out.Enabled, nil
}

// ApproveExitNode approves device id's default-route advertisement, keeping any enabled subnet routes.
func (c *Client) ApproveExitNode(ctx context.Context, deviceID string) error {
	cur, err := c.Routes(ctx, deviceID)
	if err != nil {
		return err
	}
	_, err = c.EnableRoutes(ctx, deviceID, cur, defaultRoutes)
	return err
}
