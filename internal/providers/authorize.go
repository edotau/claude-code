package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

// Authorize sets the provider's auth header and static headers on h. Passthrough leaves client auth untouched.
func Authorize(ctx context.Context, p *Provider, h http.Header) error {
	for k, v := range p.Headers {
		h.Set(k, v)
	}
	cred, err := Credential(ctx, p)
	if errors.Is(err, ErrPassthrough) {
		return nil
	}
	if err != nil {
		return err
	}
	SetCredential(p, h, cred)
	return nil
}

// SetCredential strips every client auth header from h, then sets the provider's auth header from cred.
func SetCredential(p *Provider, h http.Header, cred string) {
	h.Del("Authorization")
	h.Del("X-Api-Key")
	h.Del("X-Goog-Api-Key")
	if p.Auth.Type == AuthNone {
		for k, v := range p.Headers {
			h.Set(k, v)
		}
		return
	}
	switch p.Auth.HeaderStyle() {
	case HeaderXAPIKey:
		h.Set("X-Api-Key", cred)
	case HeaderBearer:
		h.Set("Authorization", "Bearer "+cred)
	}
}

// anthropicOAuthBeta is the header Claude Code's own client sends alongside a setup-token bearer.
const anthropicOAuthBeta = "oauth-2025-04-20"

// LiveModels GETs the provider's model catalog on whichever route it has (Anthropic /v1/models, else OpenAI
// /models). A passthrough provider (subscription) has no credential this command can mint on its own, so it
// tries the stored setup-token as a bearer credential; ErrPassthrough means even that is unavailable, and the
// caller falls back to the registry's own configured models.
func LiveModels(ctx context.Context, p *Provider) ([]string, error) {
	entries, err := liveModelEntries(ctx, p)
	ids := make([]string, len(entries))
	for i, e := range entries {
		ids[i] = e.id
	}
	return ids, err
}

// liveModelEntry is one catalog row: the id plus the display name when the API sends one (Anthropic does);
// task and ready come only from a Databricks serving-endpoints listing.
type liveModelEntry struct{ id, name, task, ready string }

func liveModelEntries(ctx context.Context, p *Provider) ([]liveModelEntry, error) {
	url, anthropicDialect := "", false
	if host := PresetHost(p.Host); p.Preset == PresetDatabricks && host != "" {
		url = host + "/api/2.0/serving-endpoints"
	} else if r, ok := p.Route(ProtocolAnthropic); ok {
		url, anthropicDialect = r+"/v1/models", true
	} else if r, ok := p.Route(ProtocolOpenAI); ok {
		url = r + "/models"
	} else {
		return nil, fmt.Errorf("%s has no route that lists models", p.Name)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if anthropicDialect {
		req.Header.Set("anthropic-version", "2023-06-01") // required by api.anthropic.com on every direct call
	}
	if p.Auth.Type == AuthCLI {
		tok := HeadlessOAuthToken()
		if tok == "" {
			return nil, ErrPassthrough
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("anthropic-beta", anthropicOAuthBeta)
	} else if err := Authorize(ctx, p, req.Header); err != nil {
		return nil, err
	}
	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s: %s", url, resp.Status, strings.TrimSpace(string(body)))
	}
	if p.Preset == PresetDatabricks && strings.HasSuffix(url, "/api/2.0/serving-endpoints") {
		return decodeServingEndpoints(url, body)
	}
	var doc struct {
		Data []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("decode %s: %w", url, err)
	}
	out := make([]liveModelEntry, len(doc.Data))
	for i, m := range doc.Data {
		out[i] = liveModelEntry{id: m.ID, name: m.DisplayName}
	}
	return out, nil
}

// decodeServingEndpoints maps a Databricks /api/2.0/serving-endpoints body to catalog rows (name, task, ready).
func decodeServingEndpoints(url string, body []byte) ([]liveModelEntry, error) {
	var doc struct {
		Endpoints []struct {
			Name  string `json:"name"`
			Task  string `json:"task"`
			State struct {
				Ready string `json:"ready"`
			} `json:"state"`
		} `json:"endpoints"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("decode %s: %w", url, err)
	}
	out := make([]liveModelEntry, len(doc.Endpoints))
	for i, e := range doc.Endpoints {
		out[i] = liveModelEntry{id: e.Name, task: e.Task, ready: e.State.Ready}
	}
	return out, nil
}

// ProbeCredential makes one authenticated catalog GET to tell a present credential from a working one:
// "login" (passthrough: not probeable here), "verified", "rejected" (401/403), "unreachable" (no connection),
// or "unverified" with the error. Never fatal; callers surface the status next to the credential.
func ProbeCredential(ctx context.Context, p *Provider) (status, detail string) {
	if p.Auth.Type == AuthCLI || p.Auth.Login != "" {
		return "login", ""
	}
	_, err := LiveModels(ctx, p)
	switch {
	case err == nil:
		return "verified", ""
	case strings.Contains(err.Error(), " 401 ") || strings.Contains(err.Error(), " 403 ") ||
		strings.Contains(err.Error(), ": 401") || strings.Contains(err.Error(), ": 403"):
		return "rejected", firstLine(err.Error())
	default:
		var ne net.Error
		if errors.As(err, &ne) || strings.Contains(err.Error(), "connection refused") || strings.Contains(err.Error(), "no such host") {
			return "unreachable", firstLine(err.Error())
		}
		return "unverified", firstLine(err.Error())
	}
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}
