package providers

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/network/httpx"
	"github.com/edotau/claude-code/internal/textutil"
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

// LiveModels GETs the provider's model catalog: Databricks serving-endpoints, else Anthropic /v1/models, else OpenAI
// /models. A passthrough provider (subscription) has no credential this command can mint on its own, so it
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
	url, anthropicDialect, err := catalogURL(p)
	if err != nil {
		return nil, err
	}
	hdr := http.Header{}
	if anthropicDialect {
		hdr.Set("anthropic-version", "2023-06-01") // required by api.anthropic.com on every direct call
	}
	status, body, _, readErr := httpx.Do(ctx, httpx.Call{Client: httpClient(), URL: url, Header: hdr, Limit: 8 << 20,
		Authorize: func(h http.Header) error { return authorizeCatalog(ctx, p, h) }})
	if status == 0 {
		return nil, readErr
	}
	if status != http.StatusOK {
		return nil, &catalogStatusError{url: url, status: fmt.Sprintf("%d %s", status, http.StatusText(status)), code: status, body: strings.TrimSpace(string(body))}
	}
	if readErr != nil { // a truncated body must not decode as a complete list
		return nil, fmt.Errorf("read %s: %w", url, readErr)
	}
	if p.Preset == PresetDatabricks && strings.HasSuffix(url, "/api/2.0/serving-endpoints") {
		return decodeServingEndpoints(url, body)
	}
	return decodeModelsList(url, body)
}

// catalogURL picks the model-listing URL for p and whether it speaks the Anthropic dialect.
func catalogURL(p *Provider) (url string, anthropicDialect bool, err error) {
	if host := PresetHost(p.Host); p.Preset == PresetDatabricks && host != "" {
		return host + "/api/2.0/serving-endpoints", false, nil
	}
	if r, ok := p.Route(ProtocolAnthropic); ok {
		return r + "/v1/models", true, nil
	}
	if r, ok := p.Route(ProtocolOpenAI); ok {
		return r + "/models", false, nil
	}
	return "", false, fmt.Errorf("%s has no route that lists models", p.Name)
}

// authorizeCatalog sets the credential headers on a catalog GET: the setup-token for a passthrough provider.
func authorizeCatalog(ctx context.Context, p *Provider, h http.Header) error {
	if p.Auth.Type != AuthCLI {
		return Authorize(ctx, p, h)
	}
	tok := HeadlessOAuthToken()
	if tok == "" {
		return ErrPassthrough
	}
	h.Set("Authorization", "Bearer "+tok)
	h.Set("anthropic-beta", anthropicOAuthBeta)
	return nil
}

// decodeModelsList decodes an Anthropic/OpenAI {"data":[{id,display_name}]} listing.
func decodeModelsList(url string, body []byte) ([]liveModelEntry, error) {
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

// catalogStatusError is a non-200 catalog GET; ProbeCredential classifies by code, and a 403 by its body's markers.
type catalogStatusError struct {
	url, status, body string
	code              int
}

func (e *catalogStatusError) Error() string {
	return fmt.Sprintf("GET %s: %s: %s", e.url, e.status, redactDetail(e.body))
}

// rejectMarkers are the 403 body fragments that mean the credential itself was refused, not a policy or WAF block.
var rejectMarkers = []string{"invalid_grant", "invalid_client", "invalid_token", "Invalid access token"}

// rejected reports a 401, or a 403 whose body names a refused credential.
func (e *catalogStatusError) rejected() bool {
	if e.code == http.StatusUnauthorized {
		return true
	}
	return e.code == http.StatusForbidden && slices.ContainsFunc(rejectMarkers, func(m string) bool { return strings.Contains(e.body, m) })
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
// "login" (passthrough: not probeable here), "verified", "rejected" (401, or a 403 naming a refused credential),
// "unreachable" (no connection),
// or "unverified" with the error. Never fatal; callers surface the status next to the credential.
func ProbeCredential(ctx context.Context, p *Provider) (status, detail string) {
	if p.Auth.Type == AuthCLI || p.Auth.Login != "" {
		return "login", ""
	}
	_, err := LiveModels(ctx, p)
	if err != nil {
		detail = textutil.CapRunes(textutil.FirstLine(err.Error()), 160, "…")
	}
	var se *catalogStatusError
	switch {
	case err == nil:
		return "verified", ""
	case errors.As(err, &se) && se.rejected():
		return "rejected", detail
	default:
		var ne net.Error
		if errors.As(err, &ne) || strings.Contains(err.Error(), "connection refused") || strings.Contains(err.Error(), "no such host") {
			return "unreachable", detail
		}
		return "unverified", detail
	}
}

// ModelInfo is one live catalog entry: ID is always set, UseCase always filled (models.UseCase never
// returns ""); Name and Capabilities are blank where the backend has nothing to report.
type ModelInfo struct {
	ID           string
	Name         string
	UseCase      string
	Capabilities string // e.g. "vision · reasoning · x2"; blank when none
	Task         string // Databricks serving endpoints only, e.g. "llm/v1/chat"
	Ready        string // Databricks serving endpoints only, e.g. "READY"
	Gateway      bool   // listed by the AI Gateway (gatewayModels), not the serving-endpoints catalog
}

// ListModels lists p's live catalog with names/use-case/capabilities when the backend reports them.
// The github row is listed by copilot.ListModels, not here.
func ListModels(ctx context.Context, p *Provider) ([]ModelInfo, error) {
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

// gatewayModels lists the Anthropic models a Databricks workspace's AI Gateway serves (display names included),
// through DiscoverModels' pager. Non-Databricks providers return nil, nil.
func gatewayModels(ctx context.Context, p *Provider) ([]ModelInfo, error) {
	host := PresetHost(p.Host)
	if p.Preset != PresetDatabricks || host == "" {
		return nil, nil
	}
	return discoverPages(ctx, p, "databricks-ai-gateway", host+"/ai-gateway/anthropic/v1/models?limit=1000")
}

// httpClient is http.DefaultClient (so its Transport stays swappable) with a timeout bounding a hung endpoint.
func httpClient() *http.Client {
	c := *http.DefaultClient
	c.Timeout = 30 * time.Second
	return &c
}
