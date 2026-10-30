package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/edotau/claude-code/internal/models"
)

// GatewayModels lists the Anthropic models a Databricks workspace's AI Gateway serves (display names included),
// following has_more/after_id paging. Non-Databricks providers return nil, nil.
func GatewayModels(ctx context.Context, p *Provider) ([]ModelInfo, error) {
	host := presetHost(p.Host)
	if p.Preset != PresetDatabricks || host == "" {
		return nil, nil
	}
	var out []ModelInfo
	after := ""
	for range 20 {
		u := host + "/ai-gateway/anthropic/v1/models?limit=1000"
		if after != "" {
			u += "&after_id=" + url.QueryEscape(after)
		}
		body, err := gatewayGet(ctx, p, u)
		if err != nil {
			return nil, err
		}
		var doc struct {
			Data []struct {
				ID          string `json:"id"`
				DisplayName string `json:"display_name"`
			} `json:"data"`
			HasMore bool   `json:"has_more"`
			LastID  string `json:"last_id"`
		}
		if err := json.Unmarshal(body, &doc); err != nil {
			return nil, fmt.Errorf("decode %s: %w", u, err)
		}
		for _, m := range doc.Data {
			name := m.DisplayName
			if name == "" {
				name = displayName(m.ID)
			}
			out = append(out, ModelInfo{ID: m.ID, Name: name, UseCase: models.UseCase(m.ID, name), Gateway: true})
		}
		if !doc.HasMore || doc.LastID == "" {
			break
		}
		after = doc.LastID
	}
	return out, nil
}

func gatewayGet(ctx context.Context, p *Provider, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("anthropic-version", "2023-06-01")
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
		return nil, fmt.Errorf("GET %s: %s: %s", u, resp.Status, strings.TrimSpace(string(body)))
	}
	return body, nil
}
