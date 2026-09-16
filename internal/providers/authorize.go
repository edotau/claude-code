package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// LiveModels GETs the provider's model catalog on whichever route it has (Anthropic /v1/models, else OpenAI /models).
func LiveModels(ctx context.Context, p *Provider) ([]string, error) {
	url := ""
	if r, ok := p.Route(DialectAnthropic); ok {
		url = r + "/v1/models"
	} else if r, ok := p.Route(DialectOpenAI); ok {
		url = r + "/models"
	} else {
		return nil, fmt.Errorf("%s has no route that lists models", p.Name)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if err := Authorize(ctx, p, req.Header); err != nil {
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
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("decode %s: %w", url, err)
	}
	ids := make([]string, len(doc.Data))
	for i, m := range doc.Data {
		ids[i] = m.ID
	}
	return ids, nil
}
