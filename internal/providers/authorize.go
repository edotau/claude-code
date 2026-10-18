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
	h.Del("Authorization")
	h.Del("X-Api-Key")
	switch p.Auth.Type {
	case AuthAPIKey:
		h.Set("X-Api-Key", cred)
	case AuthBearer:
		h.Set("Authorization", "Bearer "+cred)
	}
	return nil
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

// liveModelEntry is one catalog row: the id plus the display name when the API sends one (Anthropic does).
type liveModelEntry struct{ id, name string }

func liveModelEntries(ctx context.Context, p *Provider) ([]liveModelEntry, error) {
	url, anthropicDialect := "", false
	if r, ok := p.Route(DialectAnthropic); ok {
		url, anthropicDialect = r+"/v1/models", true
	} else if r, ok := p.Route(DialectOpenAI); ok {
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
	if p.Auth.Type == AuthPassthrough {
		tok := HeadlessOAuthToken()
		if tok == "" {
			return nil, ErrPassthrough
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("anthropic-beta", anthropicOAuthBeta)
	} else if err := Authorize(ctx, p, req.Header); err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s: %s", url, resp.Status, strings.TrimSpace(string(body)))
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

// ProbeCredential makes one authenticated catalog GET to tell a present credential from a working one:
// "login" (passthrough: not probeable here), "verified", "rejected" (401/403), "unreachable" (no connection),
// or "unverified" with the error. Never fatal; callers surface the status next to the credential.
func ProbeCredential(ctx context.Context, p *Provider) (status, detail string) {
	if p.Auth.Type == AuthPassthrough || p.Auth.Login != "" {
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
