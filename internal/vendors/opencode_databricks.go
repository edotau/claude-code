package vendors

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
)

// opencodeDatabricks is the interactive databricks launch: the curated menu split over a chat provider
// (openai-compatible), a Responses provider (<id>-gpt, GPT-6 rejects chat tool calls) and a native Anthropic one (<id>-claude).
func opencodeDatabricks(p *providers.Provider, id, dialect, base, model, cred string, routed bool, env []string) ([]string, []string, error) { // env already holds the key
	chatBase, _ := p.Route(providers.ProtocolOpenAI) // Route resolves relative routes and the preset's Protocol/BaseURL
	respBase, _ := p.Route(providers.ProtocolResponses)
	claudeBase := ""
	if r, ok := p.Route(providers.ProtocolAnthropic); ok {
		claudeBase = r + "/v1"
	}
	if routed { // base is the session base plus the dialect suffix routerRoute added
		spec, _ := Lookup("opencode")
		sfx := spec.RouterSuffix
		sess := strings.TrimSuffix(base, sfx[dialect])
		chatBase, respBase, claudeBase = sess+sfx[providers.ProtocolOpenAI], sess+sfx[providers.ProtocolResponses], sess+sfx[providers.ProtocolAnthropic]+"/v1"
	}
	if chatBase == "" || respBase == "" {
		return nil, nil, fmt.Errorf("opencode: provider %s lacks an openai or responses route", p.Name)
	}
	ref := "{env:" + OpenCodeKeyEnv + "}"
	opts := func(b string, bearer bool) map[string]any {
		o := map[string]any{"baseURL": b}
		headers := map[string]any{}
		for k, v := range p.Headers {
			headers[k] = v
		}
		if cred != "" {
			o["apiKey"] = ref
			if bearer && p.Auth.HeaderStyle() == providers.HeaderBearer {
				headers["Authorization"] = "Bearer " + ref
			}
		}
		if len(headers) > 0 {
			o["headers"] = headers
		}
		return o
	}
	chat, resp, claude := map[string]any{}, map[string]any{}, map[string]any{}
	allowed := map[string]bool{}
	for _, m := range providers.OpencodeMenu() {
		wire := menuWire(p, m.ID) // the catalog spells the id the way this row serves it
		allowed[wire] = true
		entry := map[string]any{"name": m.Label + " · " + p.Name}
		switch {
		case m.Responses:
			resp[wire] = entry
		case claudeBase != "" && models.FamilyKind(m.ID) == models.FamilyClaude:
			claude[wire] = claudeEntry(entry)
		default:
			chat[wire] = entry
		}
	}
	for _, slot := range models.Slots {
		if s := models.Strip1M(p.Models[slot]); s != "" {
			allowed[s], allowed[p.WireModel(s)] = true, true
		}
	}
	defID := id
	switch {
	case resp[model] != nil:
		defID += "-gpt"
	case claude[model] != nil:
		defID += "-claude"
	case chat[model] == nil:
		if !allowed[model] {
			return nil, nil, fmt.Errorf("opencode: model %q is not on the %s menu; see `claude-code opencode catalog list --live` or `claude-code opencode catalog add`", model, p.Name)
		}
		if claudeBase != "" && models.FamilyKind(model) == models.FamilyClaude { // a tier slot off the curated menu
			claude[model], defID = claudeEntry(map[string]any{"name": model}), defID+"-claude"
		} else {
			chat[model] = map[string]any{"name": model}
		}
	}
	prov := map[string]any{
		id:          map[string]any{"npm": "@ai-sdk/openai-compatible", "name": p.Name + " (claude-code)", "options": opts(chatBase, false), "models": chat},
		id + "-gpt": map[string]any{"npm": "@ai-sdk/openai", "name": p.Name + " GPT (claude-code)", "options": opts(respBase, false), "models": resp},
	}
	if claudeBase != "" {
		prov[id+"-claude"] = map[string]any{"npm": "@ai-sdk/anthropic", "name": p.Name + " Claude (claude-code)", "options": opts(claudeBase, true), "models": claude}
	}
	body, err := json.Marshal(map[string]any{"$schema": "https://opencode.ai/config.json", "model": defID + "/" + model, "provider": prov})
	if err != nil {
		return nil, nil, err
	}
	return append(env, "OPENCODE_CONFIG_CONTENT="+string(body)), nil, nil
}

// claudeEntry marks a Claude menu entry: no tool streaming (the router cannot relay it).
func claudeEntry(entry map[string]any) map[string]any {
	entry["options"] = map[string]any{"toolStreaming": false}
	return entry
}
