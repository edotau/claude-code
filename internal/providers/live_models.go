package providers

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/models"
)

// ModelInfo is one live catalog entry: ID is always set, UseCase always filled (models.UseCase never
// returns ""); Name and Capabilities are blank where the backend has nothing to report.
type ModelInfo struct {
	ID           string
	Name         string
	UseCase      string
	Capabilities string // e.g. "vision · reasoning · x2"; blank when none
	Task         string // Databricks serving endpoints only, e.g. "llm/v1/chat"
	Ready        string // Databricks serving endpoints only, e.g. "READY"
	Gateway      bool   // listed by the AI Gateway (GatewayModels), not the serving-endpoints catalog
}

// ListModels lists p's live catalog with names/use-case/capabilities when the backend reports them.
// The github row is listed by copilot.ListModels, not here. reg is accepted for callers that already have it in
// hand; this lister itself does not need it.
func ListModels(ctx context.Context, reg *Registry, p *Provider) ([]ModelInfo, error) {
	entries, err := liveModelEntries(ctx, p)
	if err != nil {
		return nil, err
	}
	out := make([]ModelInfo, len(entries))
	for i, e := range entries {
		name := cmp.Or(e.name, displayName(e.id))
		caps := endpointCapabilities(e)
		out[i] = ModelInfo{ID: e.id, Name: name, UseCase: models.UseCase(e.id, name), Capabilities: caps, Task: e.task, Ready: e.ready}
	}
	return out, nil
}

// endpointCapabilities joins a Databricks endpoint's task and ready state, e.g. "llm/v1/chat · READY"; "" otherwise.
func endpointCapabilities(e liveModelEntry) string {
	return strings.Join(slices.DeleteFunc([]string{e.task, e.ready}, func(s string) bool { return s == "" }), " · ")
}

// displayName derives a readable name from an id when the catalog sends none: models/gemini-2.5-flash → Gemini 2.5 Flash.
func displayName(id string) string {
	parts := strings.FieldsFunc(strings.TrimPrefix(id, "models/"), func(r rune) bool { return r == '-' || r == '_' })
	for i, w := range parts {
		if w != "" && w[0] >= 'a' && w[0] <= 'z' {
			parts[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(parts, " ")
}

// GatewayModels lists the Anthropic models a Databricks workspace's AI Gateway serves (display names included),
// following has_more/after_id paging. Non-Databricks providers return nil, nil.
func GatewayModels(ctx context.Context, p *Provider) ([]ModelInfo, error) {
	host := PresetHost(p.Host)
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

// httpClient is http.DefaultClient (so its Transport stays swappable) with a timeout bounding a hung endpoint.
func httpClient() *http.Client {
	c := *http.DefaultClient
	c.Timeout = 30 * time.Second
	return &c
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
	resp, err := httpClient().Do(req)
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
