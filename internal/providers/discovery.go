package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/models"
)

// DiscoverySource names the live adapter or explains why a vendor login is unsupported.
func DiscoverySource(p *Provider) (source, skip string) {
	if p.Provider == "github" {
		return "copilot-sdk", ""
	}
	if p.Auth.Type == AuthCLI && (p.Provider != "anthropic" || p.Auth.Login != "") {
		return "", "vendor CLI login has no supported live catalog adapter"
	}
	if p.Preset == PresetDatabricks {
		if p.IsAIGateway() {
			return "databricks-ai-gateway", ""
		}
		return "databricks-serving-endpoints", ""
	}
	if _, ok := p.Route(ProtocolGemini); ok {
		return "gemini-models", ""
	}
	if _, ok := p.Route(ProtocolAnthropic); ok {
		return "anthropic-models", ""
	}
	if _, ok := p.Route(ProtocolOpenAI); ok {
		return "openai-models", ""
	}
	if _, ok := p.Route(ProtocolResponses); ok {
		return "openai-models", ""
	}
	return "", "profile has no supported live model-list route"
}

// DiscoverModels lists live only, with bounded pagination and no partial or fallback results.
func DiscoverModels(ctx context.Context, p *Provider) ([]ModelInfo, error) {
	source, skip := DiscoverySource(p)
	if skip != "" {
		return nil, errors.New(skip)
	}
	base, err := discoveryBase(p, source)
	if err != nil {
		return nil, err
	}
	var out []ModelInfo
	cursor, cursorKey := "", ""
	visited := make(map[string]bool)
	for range 100 {
		u, err := url.Parse(base)
		if err != nil {
			return nil, errors.New("invalid catalog route")
		}
		q := u.Query()
		if cursor != "" {
			q.Set(cursorKey, cursor)
		}
		u.RawQuery = q.Encode()
		body, err := discoveryGet(ctx, p, u.String(), source)
		if err != nil {
			return nil, err
		}
		doc, empty, err := decodeCatalogPage(body, source)
		if err != nil {
			return nil, err
		}
		if empty {
			return MergePickerModels(nil, out), nil
		}
		entries, err := catalogEntries(source, body, doc)
		if err != nil {
			return nil, err
		}
		out = append(out, entries...)
		next, key, err := catalogCursor(source, doc)
		if err != nil {
			return nil, err
		}
		if next == "" {
			return MergePickerModels(nil, out), nil
		}
		if visited[next] {
			return nil, errors.New("catalog pagination repeated a cursor")
		}
		visited[next], cursor, cursorKey = true, next, key
	}
	return nil, errors.New("catalog pagination exceeded 100 pages; no partial catalog saved")
}

// discoveryBase returns the list URL (without query) for a discovery source.
func discoveryBase(p *Provider, source string) (string, error) {
	var base string
	switch source {
	case "databricks-serving-endpoints":
		base = PresetHost(p.Host) + "/api/2.0/serving-endpoints"
	case "databricks-ai-gateway":
		base, _ = p.Route(ProtocolAnthropic)
		base += "/v1/models"
	case "gemini-models":
		base, _ = p.Route(ProtocolGemini)
		if !strings.HasSuffix(base, "/v1beta") && !strings.HasSuffix(base, "/v1") {
			base += "/v1beta"
		}
		base += "/models"
	case "anthropic-models":
		base, _ = p.Route(ProtocolAnthropic)
		base += "/v1/models"
	case "openai-models":
		var ok bool
		base, ok = p.Route(ProtocolOpenAI)
		if !ok {
			base, _ = p.Route(ProtocolResponses)
		}
		base += "/models"
	default:
		return "", errors.New("catalog requires a separate account-specific adapter")
	}
	return base, nil
}

type catalogDoc struct {
	Data []struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
	} `json:"data"`
	HasMore       bool   `json:"has_more"`
	LastID        string `json:"last_id"`
	NextPageToken string `json:"next_page_token"`
	GeminiNext    string `json:"nextPageToken"`
	Models        []struct {
		Name        string   `json:"name"`
		DisplayName string   `json:"displayName"`
		Methods     []string `json:"supportedGenerationMethods"`
	} `json:"models"`
}

// decodeCatalogPage validates one page; empty reports a bare {} from a source that omits empty arrays.
func decodeCatalogPage(body []byte, source string) (doc catalogDoc, empty bool, err error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil || fields == nil {
		return doc, false, errors.New("invalid catalog JSON object")
	}
	array := "data"
	switch source {
	case "databricks-serving-endpoints":
		array = "endpoints"
	case "gemini-models":
		array = "models"
	}
	// Protobuf JSON omits empty repeated fields for Gemini and serving endpoints.
	if _, ok := fields[array]; !ok && len(fields) == 0 && array != "data" {
		return doc, true, nil
	}
	raw := strings.TrimSpace(string(fields[array]))
	if len(raw) == 0 || raw[0] != '[' || fields["error"] != nil || fields["error_code"] != nil {
		return doc, false, errors.New("catalog response missing expected model array or contains an error")
	}
	if json.Unmarshal(body, &doc) != nil {
		return doc, false, errors.New("invalid catalog JSON response")
	}
	return doc, false, nil
}

// catalogEntries converts one decoded page into models for the source.
func catalogEntries(source string, body []byte, doc catalogDoc) ([]ModelInfo, error) {
	var out []ModelInfo
	switch source {
	case "databricks-serving-endpoints":
		entries, err := decodeServingEndpoints("catalog", body)
		if err != nil {
			return nil, errors.New("invalid serving endpoints response")
		}
		for _, e := range entries {
			if strings.TrimSpace(e.id) == "" {
				return nil, errors.New("catalog entry missing model ID")
			}
			out = append(out, ModelInfo{ID: e.id, Name: displayName(e.id), UseCase: models.UseCase(e.id, e.name), Task: e.task, Ready: e.ready})
		}
	case "gemini-models":
		for _, m := range doc.Models {
			if strings.TrimSpace(strings.TrimPrefix(m.Name, "models/")) == "" {
				return nil, errors.New("catalog entry missing model ID")
			}
			if !slices.Contains(m.Methods, "generateContent") {
				continue
			}
			// generateContent clients take the id without the resource's models/ prefix.
			id := strings.TrimPrefix(m.Name, "models/")
			out = append(out, ModelInfo{ID: id, Name: m.DisplayName, UseCase: models.UseCase(id, m.DisplayName)})
		}
	default:
		for _, m := range doc.Data {
			if strings.TrimSpace(m.ID) == "" {
				return nil, errors.New("catalog entry missing model ID")
			}
			name := m.DisplayName
			if name == "" {
				name = displayName(m.ID)
			}
			out = append(out, ModelInfo{ID: m.ID, Name: name, UseCase: models.UseCase(m.ID, name), Gateway: source == "databricks-ai-gateway"})
		}
	}
	return out, nil
}

// catalogCursor returns the next-page cursor and its query key; empty next means the last page.
func catalogCursor(source string, doc catalogDoc) (next, key string, err error) {
	next, key = doc.NextPageToken, "page_token"
	if source == "gemini-models" {
		next, key = doc.GeminiNext, "pageToken"
	}
	if !doc.HasMore {
		return next, key, nil
	}
	next, key = doc.LastID, "after_id"
	if source == "openai-models" {
		key = "after"
	}
	if next == "" && len(doc.Data) > 0 {
		next = doc.Data[len(doc.Data)-1].ID
	}
	if next == "" {
		return "", "", errors.New("catalog pagination has no cursor")
	}
	return next, key, nil
}

func discoveryGet(ctx context.Context, p *Provider, u, source string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, errors.New("cannot construct catalog request")
	}
	req.Header.Set("anthropic-version", "2023-06-01")
	if p.Auth.Type == AuthCLI {
		tok := HeadlessOAuthToken()
		if tok == "" {
			return nil, errors.New("no harness-readable subscription credential")
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("anthropic-beta", anthropicOAuthBeta)
	} else if err := Authorize(ctx, p, req.Header); err != nil {
		return nil, errors.New("catalog authentication unavailable; check profile credentials")
	}
	if source == "gemini-models" && p.Auth.Type == AuthKey {
		token, err := Credential(ctx, p)
		if err != nil {
			return nil, errors.New("gemini API credential unavailable")
		}
		req.Header.Del("Authorization")
		req.Header.Del("X-Api-Key")
		req.Header.Set("X-Goog-Api-Key", token)
	}
	client := httpClient()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("catalog request failed or timed out; check connectivity")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("catalog request failed (HTTP %d)", resp.StatusCode)
	}
	const limit = 8 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || len(body) > limit {
		return nil, errors.New("cannot read bounded catalog response")
	}
	return body, nil
}
