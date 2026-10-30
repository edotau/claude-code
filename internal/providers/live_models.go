package providers

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/copilotsdk"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
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
// github-copilot goes through its own SDK (its own GitHub login, never BYOK) and is the only source of
// capability flags; every other provider keeps the plain /models GET, which has no such fields. reg is
// accepted for callers that already have it in hand; this lister itself does not need it.
func ListModels(ctx context.Context, reg *Registry, p *Provider) ([]ModelInfo, error) {
	if p.provider() == "github" {
		return liveCopilotModelInfos(ctx)
	}
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

// CopilotTokenFn resolves the launch-account token (vendors.CopilotToken) for copilot catalog calls.
// providers cannot import internal/agent/vendors directly (vendors already imports providers); vendors sets
// this at init time instead. nil or "" falls back to the copilot CLI's own stored login.
var CopilotTokenFn func() string

func liveCopilotModelInfos(ctx context.Context) ([]ModelInfo, error) {
	bin, err := paths.VendorBin("copilot")
	if err != nil {
		return nil, fmt.Errorf("copilot CLI not found: %w", err)
	}
	var tok string
	if CopilotTokenFn != nil {
		tok = CopilotTokenFn()
	}
	list, err := copilotsdk.ListModels(ctx, copilotsdk.CatalogOptions(bin, tok))
	if err != nil {
		return nil, err
	}
	out := make([]ModelInfo, len(list))
	for i, m := range list {
		out[i] = ModelInfo{ID: m.ID, Name: m.Name, UseCase: models.UseCase(m.ID, m.Name), Capabilities: copilotCapabilities(m)}
	}
	return out, nil
}

// copilotCapabilities joins copilot's capability flags and premium-billing multiplier into one
// "vision · reasoning · x2" string; "" when the model carries none of them.
func copilotCapabilities(m copilotsdk.Model) string {
	var parts []string
	if m.Vision {
		parts = append(parts, "vision")
	}
	if m.ReasoningEffort {
		parts = append(parts, "reasoning")
	}
	if m.Multiplier != nil {
		parts = append(parts, fmt.Sprintf("x%g", *m.Multiplier))
	}
	return strings.Join(parts, " · ")
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
