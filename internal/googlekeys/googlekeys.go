// Package googlekeys mints, looks up and deletes Gemini API keys through the Google Cloud API Keys v2 REST API,
// authenticated by Application Default Credentials (`gcloud auth application-default login`). Stdlib only: the
// apikeys Go SDK wraps these same four calls but pulls in gRPC and protobuf.
package googlekeys

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// GeminiService is the only API a minted key may call (least privilege).
const GeminiService = "generativelanguage.googleapis.com"

// Endpoints are package vars so tests can point them at an httptest server.
var (
	TokenURL   = "https://oauth2.googleapis.com/token"
	APIKeysURL = "https://apikeys.googleapis.com/v2"
)

// ErrNoADC means no Application Default Credentials file exists; the caller should offer the gcloud login.
var ErrNoADC = errors.New("no Google Cloud credentials (run `gcloud auth application-default login`)")

// LoginArgv is the browser sign-in that writes the ADC file this package reads.
var LoginArgv = []string{"gcloud", "auth", "application-default", "login"}

// adc is the authorized_user shape gcloud writes; a service_account file is refused (no JWT signing here).
type adc struct {
	Type           string `json:"type"`
	ClientID       string `json:"client_id"`
	ClientSecret   string `json:"client_secret"`
	RefreshToken   string `json:"refresh_token"`
	QuotaProjectID string `json:"quota_project_id"`
}

// ADCPath is $GOOGLE_APPLICATION_CREDENTIALS, else gcloud's well-known file.
func ADCPath() string {
	if p := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"); p != "" {
		return p
	}
	if dir := os.Getenv("CLOUDSDK_CONFIG"); dir != "" {
		return filepath.Join(dir, "application_default_credentials.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "gcloud", "application_default_credentials.json")
}

// Client calls the API Keys API for one project with a refreshed access token.
type Client struct {
	HTTP    *http.Client
	Project string
	token   string
}

// New loads ADC, exchanges its refresh token for an access token, and resolves the project
// ($GOOGLE_CLOUD_PROJECT, else the ADC quota project).
func New(ctx context.Context) (*Client, error) {
	b, err := os.ReadFile(ADCPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoADC
	}
	if err != nil {
		return nil, err
	}
	var cred adc
	if err := json.Unmarshal(b, &cred); err != nil {
		return nil, fmt.Errorf("parse %s: %w", ADCPath(), err)
	}
	if cred.Type != "authorized_user" || cred.RefreshToken == "" {
		return nil, fmt.Errorf("%s is %q credentials; only a gcloud user login (authorized_user) is supported", ADCPath(), cred.Type)
	}
	project := strings.TrimSpace(os.Getenv("GOOGLE_CLOUD_PROJECT"))
	if project == "" {
		project = cred.QuotaProjectID
	}
	if project == "" {
		return nil, errors.New("no Google Cloud project: set GOOGLE_CLOUD_PROJECT or run `gcloud auth application-default set-quota-project <id>`")
	}
	c := &Client{HTTP: &http.Client{Timeout: 20 * time.Second}, Project: project}
	tok, err := c.refresh(ctx, cred)
	if err != nil {
		return nil, err
	}
	c.token = tok
	return c, nil
}

func (c *Client) refresh(ctx context.Context, cred adc) (string, error) {
	form := url.Values{"grant_type": {"refresh_token"}, "client_id": {cred.ClientID},
		"client_secret": {cred.ClientSecret}, "refresh_token": {cred.RefreshToken}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := c.do(req, &out); err != nil {
		return "", fmt.Errorf("refresh Google credentials: %w", err)
	}
	if out.AccessToken == "" {
		return "", errors.New("refresh Google credentials: no access_token in response")
	}
	return out.AccessToken, nil
}

// do sends req and decodes a 2xx JSON body into out; a non-2xx returns Google's error message, never the token.
func (c *Client) do(req *http.Request, out any) error {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
			Description string `json:"error_description"`
		}
		_ = json.Unmarshal(body, &e)
		msg := e.Error.Message
		if msg == "" {
			msg = e.Description
		}
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, msg)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}

func (c *Client) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, APIKeysURL+"/"+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("x-goog-user-project", c.Project) // bill the call to the user's project, not gcloud's client
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.do(req, out)
}

type operation struct {
	Name     string          `json:"name"`
	Done     bool            `json:"done"`
	Response json.RawMessage `json:"response"`
	Error    *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// wait polls a long-running operation until done (or ctx ends) and returns its response.
func (c *Client) wait(ctx context.Context, op operation) (json.RawMessage, error) {
	for !op.Done {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
		if err := c.call(ctx, http.MethodGet, op.Name, nil, &op); err != nil {
			return nil, err
		}
	}
	if op.Error != nil {
		return nil, errors.New(op.Error.Message)
	}
	return op.Response, nil
}

// Create mints a key restricted to the Gemini API and returns its resource name and plaintext value.
func (c *Client) Create(ctx context.Context, displayName string) (name, keyString string, err error) {
	in := map[string]any{
		"displayName":  displayName,
		"restrictions": map[string]any{"apiTargets": []map[string]string{{"service": GeminiService}}},
	}
	var op operation
	if err := c.call(ctx, http.MethodPost, "projects/"+url.PathEscape(c.Project)+"/locations/global/keys", in, &op); err != nil {
		return "", "", fmt.Errorf("create key: %w", err)
	}
	resp, err := c.wait(ctx, op)
	if err != nil {
		return "", "", fmt.Errorf("create key: %w", err)
	}
	var key struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(resp, &key); err != nil || key.Name == "" {
		return "", "", errors.New("create key: no key name in the operation response")
	}
	var ks struct {
		KeyString string `json:"keyString"`
	}
	if err := c.call(ctx, http.MethodGet, key.Name+"/keyString", nil, &ks); err != nil {
		return key.Name, "", fmt.Errorf("read new key: %w", err)
	}
	return key.Name, ks.KeyString, nil
}

// Lookup resolves a plaintext key to its resource name (keys:lookupKey), so a rotation can delete the old one.
func (c *Client) Lookup(ctx context.Context, keyString string) (string, error) {
	var out struct {
		Name string `json:"name"`
	}
	if err := c.call(ctx, http.MethodGet, "keys:lookupKey?keyString="+url.QueryEscape(keyString), nil, &out); err != nil {
		return "", err
	}
	return out.Name, nil
}

// Delete soft-deletes a key (restorable for 30 days) and waits for the operation.
func (c *Client) Delete(ctx context.Context, name string) error {
	var op operation
	if err := c.call(ctx, http.MethodDelete, name, nil, &op); err != nil {
		return err
	}
	_, err := c.wait(ctx, op)
	return err
}
