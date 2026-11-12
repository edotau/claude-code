package vscode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/databricks/serving"
	"github.com/edotau/claude-code/internal/gateway"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/textutil"
)

const (
	modelsFileName = "chatLanguageModels.json"
	groupPrefix    = "Databricks "                // groups this merge owns; every other provider is the user's
	proxyKeyRef    = "${input:harnessProxyToken}" // VS Code prompts once and keeps the value in its secret storage
	maxOutputCap   = 32_000                       // the output cap current Claude/GPT tiers accept (copilot's too)
	vendorEndpoint = "customendpoint"
)

// wireAPI maps a router wire to VS Code's apiType and the full /p/<profile> path that serves it.
var wireAPI = map[string]struct{ apiType, path string }{
	providers.ProtocolAnthropic: {"messages", "/v1/messages"},
	providers.ProtocolOpenAI:    {"chat-completions", "/" + providers.ProtocolOpenAI + "/chat/completions"},
	providers.ProtocolResponses: {"responses", "/" + providers.ProtocolResponses + "/responses"},
}

// endpointModel is one chatLanguageModels.json model entry (Custom Endpoint reference).
type endpointModel struct {
	ID                string            `json:"id"`
	Name              string            `json:"name"`
	URL               string            `json:"url"`
	APIType           string            `json:"apiType"`
	ToolCalling       bool              `json:"toolCalling"`
	Vision            bool              `json:"vision"`
	ContextWindow     int               `json:"contextWindow"`
	MaxOutputTokens   int               `json:"maxOutputTokens"`
	StatefulResponses *bool             `json:"statefulResponses,omitempty"`
	RequestHeaders    map[string]string `json:"requestHeaders"`
}

// endpointGroup is one provider entry: a model-picker group.
type endpointGroup struct {
	Name    string          `json:"name"`
	Vendor  string          `json:"vendor"`
	APIKey  string          `json:"apiKey"`
	APIType string          `json:"apiType"`
	Models  []endpointModel `json:"models"`
}

// modelGroups is what the merge writes: one group per profile, and the profiles whose listing was cold.
type modelGroups struct {
	Groups []endpointGroup
	Cold   []string // their existing group, if any, is kept as is
}

// routerBase is a seam for tests: the running proxy probe.
var routerBase = runningRouterBase

// runningRouterBase is the running proxy's base (its port may have moved), else the default port.
func runningRouterBase() string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	port := gateway.DefaultPort
	if st, ok := gateway.Running(ctx); ok {
		port = st.Port
	}
	return gateway.ViaBase(gateway.Base(port))
}

// buildDatabricksGroups lists every signed-in Databricks serving profile's cached READY chat endpoints, no network.
func buildDatabricksGroups() (modelGroups, error) {
	reg, err := providers.Load()
	if err != nil {
		return modelGroups{}, err
	}
	var out modelGroups
	base := ""
	for _, name := range reg.ProfilesOf(providers.GroupDatabricks) {
		p := reg.Providers[name]
		if reg.ServingRow(name) != nil || p.Auth.Login != "" || !providers.CredentialPresent(p) {
			continue
		}
		snap, ok := serving.Recall(p)
		if !ok {
			fmt.Fprintf(os.Stderr, "claude-code: vscode models: %s has no cached listing, group skipped "+
				"(claude-code preflight --discover %s)\n", name, name)
			out.Cold = append(out.Cold, name)
			continue
		}
		if base == "" {
			base = routerBase()
		}
		out.Groups = append(out.Groups, profileGroup(p, snap.Models, gateway.SessionBase(base, name)))
	}
	return out, nil
}

// profileGroup maps p's READY chat endpoints to one group; an endpoint p has no route for is left out.
func profileGroup(p *providers.Provider, infos []providers.ModelInfo, session string) endpointGroup {
	g := endpointGroup{Name: groupPrefix + p.Name, Vendor: vendorEndpoint, APIKey: proxyKeyRef,
		APIType: wireAPI[providers.ProtocolOpenAI].apiType, Models: []endpointModel{}}
	for _, info := range infos {
		if !info.ReadyChat() {
			continue
		}
		wire, ok, _ := providers.WireFor(p, info.ID)
		if !ok {
			continue
		}
		g.Models = append(g.Models, endpointEntry(p, info, wire, session))
	}
	return g
}

// endpointEntry is one model: the full endpoint name as id and label, routed through the proxy session base.
func endpointEntry(p *providers.Provider, info providers.ModelInfo, wire, session string) endpointModel {
	api := wireAPI[wire]
	window, output := providers.ContextWindow(p, info.ID), maxOutputCap
	if l, ok := models.Limits(info.ID); ok { // catalog limits only lower the window and the cap
		window, output = minPositive(window, l.Context), minPositive(output, l.Output)
	}
	claude := models.FamilyKind(info.ID) == models.FamilyClaude
	m := endpointModel{ID: info.ID, Name: info.ID, URL: session + api.path, APIType: api.apiType, ToolCalling: true,
		Vision: claude || strings.Contains(info.Capabilities, "vision"), ContextWindow: window, MaxOutputTokens: output,
		RequestHeaders: map[string]string{gateway.AgentHeader: "vscode"}}
	if wire == providers.ProtocolResponses {
		stateless := false // Databricks keeps no prior responses: send full history every turn
		m.StatefulResponses = &stateless
	}
	return m
}

func minPositive(a, b int) int {
	if b > 0 {
		return min(a, b)
	}
	return a
}

// syncModels merges the Databricks groups into dest, returning "<group> (<n> models)" per group; check never writes.
func syncModels(dest string, check bool) (mergeResult, []string, error) {
	mg, err := buildDatabricksGroups()
	if err != nil {
		return mergeResult{}, nil, fmt.Errorf("models: %w", err)
	}
	var summary []string
	for _, g := range mg.Groups {
		summary = append(summary, fmt.Sprintf("%s (%d models)", g.Name, len(g.Models)))
	}
	if check {
		result, err := mergeModels(dest, mg, true)
		return result, summary, err
	}
	var result mergeResult
	err = withLock("vscode-models.lock", dest, func() (mergeErr error) {
		result, mergeErr = mergeModels(dest, mg, false)
		return mergeErr
	})
	return result, summary, err
}

func mergeModels(dest string, mg modelGroups, check bool) (mergeResult, error) {
	var base []json.RawMessage
	rawBase, err := readBase(dest, "chat models", &base)
	if err != nil {
		return mergeResult{}, err
	}
	body, err := mergedModels(base, mg)
	if err != nil {
		return mergeResult{}, err
	}
	if sameJSON(rawBase, body) || (len(rawBase) == 0 && len(base) == 0 && len(mg.Groups) == 0) {
		return mergeResult{}, nil
	}
	if check {
		return mergeResult{Changed: true}, nil
	}
	return writeGuarded(dest, "chat models", rawBase, body)
}

// mergedModels keeps every foreign provider's bytes in order (and a cold profile's group), then appends the groups.
func mergedModels(base []json.RawMessage, mg modelGroups) ([]byte, error) {
	items := make([][]byte, 0, len(base)+len(mg.Groups))
	for _, raw := range base {
		var head struct{ Name string }
		_ = json.Unmarshal(raw, &head) // a non-object entry is foreign: kept
		profile, ours := strings.CutPrefix(head.Name, groupPrefix)
		if !ours || slices.Contains(mg.Cold, profile) {
			items = append(items, raw)
		}
	}
	for _, g := range mg.Groups {
		b, err := json.MarshalIndent(g, "  ", "  ")
		if err != nil {
			return nil, err
		}
		items = append(items, b)
	}
	if len(items) == 0 {
		return []byte("[]\n"), nil
	}
	return append(append([]byte("[\n  "), bytes.Join(items, []byte(",\n  "))...), "\n]\n"...), nil
}

// sameJSON reports whether raw (JSONC) and body decode to the same value.
func sameJSON(raw, body []byte) bool {
	var a, b any
	if json.Unmarshal(textutil.StripJSONC(raw), &a) != nil || json.Unmarshal(body, &b) != nil {
		return false
	}
	return reflect.DeepEqual(a, b)
}

// resolveModelsDest mirrors resolveKeybindingsDest: explicit wins, an explicit settings dest keeps its directory.
func resolveModelsDest(modelsDest, dest string) (path string, skip bool, note string, err error) {
	switch {
	case modelsDest != "":
		return modelsDest, false, "", nil
	case dest != "":
		return filepath.Join(filepath.Dir(dest), modelsFileName), false, "", nil
	}
	kb, skip, note, err := defaultKeybindingsDest() // chat models are UI-side too: the client's User dir
	if err != nil || skip {
		return "", skip, strings.ReplaceAll(note, "keybindings", "chat models"), err
	}
	return filepath.Join(filepath.Dir(kb), modelsFileName), false, "", nil
}
