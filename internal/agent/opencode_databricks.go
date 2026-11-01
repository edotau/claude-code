package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
)

// CatalogWarn receives catalog warnings (codex and opencode menus).
var CatalogWarn io.Writer = os.Stderr

// SetCatalogWarn redirects catalog warnings and returns the previous writer; tests use it to keep stderr quiet.
func SetCatalogWarn(w io.Writer) io.Writer { old := CatalogWarn; CatalogWarn = w; return old }

// MenuWire spells a bare menu id the way p serves it; an id outside the catalog still takes the row's prefix once.
func MenuWire(p *providers.Provider, id string) string {
	if w := p.WireModel(id); w != id {
		return w
	}
	rule := p.SpellingRule()
	if _, _, known := models.Resolve(id); known || rule == "" ||
		strings.HasPrefix(id, models.DatabricksPrefix) || strings.HasPrefix(id, "system.ai.") {
		return id
	}
	return models.Wire(rule, id)
}

// opencodeDatabricks is the interactive databricks launch: the curated menu split over a chat provider
// (openai-compatible), a Responses provider (<id>-gpt, GPT-6 rejects chat tool calls) and a native Anthropic one (<id>-claude).
func opencodeDatabricks(p *providers.Provider, id, dialect, base, model, cred string, routed bool, env []string) ([]string, []string, error) { // env already holds the key
	chatBase, _ := p.Route(providers.ProtocolOpenAI) // Route resolves relative routes and the preset's Protocol/BaseURL
	respBase, _ := p.Route(providers.ProtocolResponses)
	claudeBase := ""
	if r, ok := p.Route(providers.ProtocolAnthropic); ok {
		claudeBase = r + "/v1"
	}
	if respBase == "" {
		return nil, nil, fmt.Errorf("opencode: provider %s lacks a responses route", p.Name)
	}
	if routed { // base is the session base plus the dialect suffix routerRoute added
		s, _ := LookupSchema("opencode")
		sfx := s.CLI.RouterSuffix
		sess := strings.TrimSuffix(base, sfx[dialect])
		chatBase, respBase, claudeBase = sess+sfx[providers.ProtocolOpenAI], sess+sfx[providers.ProtocolResponses], sess+sfx[providers.ProtocolAnthropic]+"/v1"
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
	chat, resp, claude, allowed, dropped := splitMenu(p, claudeBase != "")
	if len(dropped) > 0 {
		fmt.Fprintf(CatalogWarn, "opencode: %s has no openai route; left out of the menu: %s\n", p.Name, strings.Join(dropped, ", "))
	}
	for _, alias := range models.AliasNames {
		if s := models.Strip1M(p.Models[alias]); s != "" {
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
		if claudeBase != "" && models.FamilyKind(model) == models.FamilyClaude { // an alias off the curated menu
			claude[model], defID = claudeEntry(map[string]any{"name": model}), defID+"-claude"
		} else {
			if _, ok, why := providers.WireFor(p, model); !ok {
				return nil, nil, fmt.Errorf("opencode: model %q: %s", model, why)
			}
			chat[model] = map[string]any{"name": model}
		}
	}
	prov := map[string]any{
		id + "-gpt": map[string]any{"npm": "@ai-sdk/openai", "name": p.Name + " GPT (claude-code)", "options": opts(respBase, false), "models": resp},
	}
	if len(chat) > 0 {
		prov[id] = map[string]any{"npm": "@ai-sdk/openai-compatible", "name": p.Name + " (claude-code)", "options": opts(chatBase, false), "models": chat}
	}
	if claudeBase != "" {
		prov[id+"-claude"] = map[string]any{"npm": "@ai-sdk/anthropic", "name": p.Name + " Claude (claude-code)", "options": opts(claudeBase, true), "models": claude}
	}
	body, err := json.Marshal(map[string]any{"$schema": "https://opencode.ai/config.json", "model": defID + "/" + model, "provider": prov})
	if err != nil {
		return nil, nil, err
	}
	return append(env, opencodeConfigEnv+"="+string(body)), nil, nil
}

// splitMenu sorts the menu over the chat, Responses and Claude providers by wire; dropped are labels p has no route for.
func splitMenu(p *providers.Provider, claudeRoute bool) (chat, resp, claude map[string]any, allowed map[string]bool, dropped []string) {
	chat, resp, claude, allowed = map[string]any{}, map[string]any{}, map[string]any{}, map[string]bool{}
	for _, m := range providers.OpencodeMenu() {
		wire := MenuWire(p, m.ID) // the catalog spells the id the way this row serves it
		allowed[wire] = true
		entry := map[string]any{"name": m.Label + " · " + p.Name}
		switch {
		case m.Responses:
			resp[wire] = entry
		case claudeRoute && models.FamilyKind(m.ID) == models.FamilyClaude:
			claude[wire] = claudeEntry(entry)
		default:
			if _, ok, _ := providers.WireFor(p, wire); !ok { // no openai route (ai-gateway): the chat endpoint would 404
				dropped = append(dropped, m.Label)
				continue
			}
			chat[wire] = entry
		}
	}
	return chat, resp, claude, allowed, dropped
}

// claudeEntry marks a Claude menu entry: no tool streaming (the router cannot relay it).
func claudeEntry(entry map[string]any) map[string]any {
	entry["options"] = map[string]any{"toolStreaming": false}
	return entry
}
