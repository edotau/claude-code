package opencode

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
)

// opencodeDatabricks splits the menu across chat, Responses (GPT-6 rejects chat tools) and native Anthropic providers.
func opencodeDatabricks(p *providers.Provider, id, dialect, base, model, cred string, routed bool, env []string) ([]string, error) { // env already holds the key
	chatBase, respBase, claudeBase, err := databricksRoutes(p, dialect, base, routed)
	if err != nil {
		return nil, err
	}
	chat, resp, claude, allowed, dropped := splitMenu(p, claudeBase != "")
	if len(dropped) > 0 {
		fmt.Fprintf(agentconfig.CatalogWarn, "opencode: %s has no openai route; left out of the menu: %s\n", p.Name, strings.Join(dropped, ", "))
	}
	for _, alias := range models.AliasNames {
		if s := models.Strip1M(p.Models[alias]); s != "" {
			allowed[s], allowed[p.WireModel(s)] = true, true
		}
	}
	defID, err := defaultProviderID(p, id, model, claudeBase != "", chat, resp, claude, allowed)
	if err != nil {
		return nil, err
	}
	prov := map[string]any{
		id + "-gpt": map[string]any{"npm": "@ai-sdk/openai", "name": "GPT (claude-code)", "options": providerOptions(p, cred, respBase, false), "models": resp},
	}
	if len(chat) > 0 {
		prov[id] = map[string]any{"npm": "@ai-sdk/openai-compatible", "name": "Databricks (claude-code)", "options": providerOptions(p, cred, chatBase, false), "models": chat}
	}
	if claudeBase != "" {
		prov[id+"-claude"] = map[string]any{"npm": "@ai-sdk/anthropic", "name": "Claude (claude-code)", "options": providerOptions(p, cred, claudeBase, true), "models": claude}
	}
	body, err := json.Marshal(map[string]any{"$schema": "https://opencode.ai/config.json", "model": defID + "/" + model, "provider": prov})
	if err != nil {
		return nil, err
	}
	return append(modelsDefined(env), opencodeConfigEnv+"="+string(body)), nil
}

// databricksRoutes is p's chat, Responses and Claude base URLs (the last "" when p has no Anthropic route).
func databricksRoutes(p *providers.Provider, dialect, base string, routed bool) (chatBase, respBase, claudeBase string, err error) {
	chatBase, _ = p.Route(providers.ProtocolOpenAI) // Route resolves relative routes and the preset's Protocol/BaseURL
	respBase, _ = p.Route(providers.ProtocolResponses)
	if r, ok := p.Route(providers.ProtocolAnthropic); ok {
		claudeBase = r + "/v1"
	}
	if respBase == "" {
		return "", "", "", fmt.Errorf("opencode: provider %s lacks a responses route", p.Name)
	}
	if routed { // base is the session base plus the dialect suffix routerRoute added
		sfx := row.CLI.RouterSuffix
		sess := strings.TrimSuffix(base, sfx[dialect])
		chatBase, respBase, claudeBase = sess+sfx[providers.ProtocolOpenAI], sess+sfx[providers.ProtocolResponses], sess+sfx[providers.ProtocolAnthropic]+"/v1"
	}
	return chatBase, respBase, claudeBase, nil
}

// providerOptions is one provider's options block: base URL, key reference and the row's headers.
func providerOptions(p *providers.Provider, cred, b string, bearer bool) map[string]any {
	ref := "{env:" + openCodeKeyEnv + "}"
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

// defaultProviderID is the provider id serving model, adding an off-menu model to its menu (chat or claude).
func defaultProviderID(p *providers.Provider, id, model string, claudeRoute bool, chat, resp, claude map[string]any, allowed map[string]bool) (string, error) {
	switch {
	case resp[model] != nil:
		return id + "-gpt", nil
	case claude[model] != nil:
		return id + "-claude", nil
	case chat[model] != nil:
		return id, nil
	}
	if !allowed[model] {
		return "", fmt.Errorf("opencode: model %q is not on the %s menu; see `claude-code opencode catalog list --live` or `claude-code opencode catalog add`", model, p.Name)
	}
	if claudeRoute && models.FamilyKind(model) == models.FamilyClaude { // an alias off the curated menu
		claude[model] = claudeEntry(map[string]any{"name": model})
		return id + "-claude", nil
	}
	if _, ok, why := providers.WireFor(p, model); !ok {
		return "", fmt.Errorf("opencode: model %q: %s", model, why)
	}
	chat[model] = map[string]any{"name": model}
	return id, nil
}

// splitMenu sorts the menu over the chat, Responses and Claude providers by wire; dropped are labels p has no route for.
func splitMenu(p *providers.Provider, claudeRoute bool) (chat, resp, claude map[string]any, allowed map[string]bool, dropped []string) {
	chat, resp, claude, allowed = map[string]any{}, map[string]any{}, map[string]any{}, map[string]bool{}
	menu, err := providers.OpencodeMenuFor(p)
	if err != nil {
		fmt.Fprintln(agentconfig.CatalogWarn, "opencode:", err)
	}
	for _, m := range menu {
		wire := agentconfig.MenuWire(p, m.ID) // the catalog spells the id the way this row serves it
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
