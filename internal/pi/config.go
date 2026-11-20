package pi

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/schemas"
)

var _ = schemas.Register(schemas.Format{
	Name: "pi-config", Owner: "internal/pi", Kind: schemas.Derived,
	Glob:    func() string { return filepath.Join(paths.StateDir(), "pi", "*", "agent", "*.json") },
	Version: 1, Clean: "pi",
})

// KeyRef is the apiKey value in every provider: Pi interpolates $NAME from its environment, so no token is on disk.
const KeyRef = "$HARNESS_PROXY_API_KEY"

// Bases are the three proxy base URLs the caller resolved for the profile; an empty one means no route.
type Bases struct{ Claude, Responses, Chat string }

type piProvider struct{ id, api, wire string }

var piProviders = []piProvider{
	{"dbx-claude", "anthropic-messages", providers.ProtocolAnthropic},
	{"dbx-gpt", "openai-responses", providers.ProtocolResponses},
	{"dbx-chat", "openai-completions", providers.ProtocolOpenAI},
}

func (b Bases) forWire(wire string) string {
	switch wire {
	case providers.ProtocolAnthropic:
		return b.Claude
	case providers.ProtocolResponses:
		return b.Responses
	}
	return b.Chat
}

// AgentDir is the directory Pi reads as PI_CODING_AGENT_DIR for profile.
func AgentDir(profile string) string {
	return filepath.Join(paths.StateDir(), "pi", profile, "agent")
}

// pickWire is the Pi wire for a model; Grok goes to chat because Databricks does not stream it over Responses.
func pickWire(menu []providers.OpencodeModel, p *providers.Provider, wire string) (string, bool) {
	w, ok, _ := providers.WireForIn(menu, p, wire)
	if ok && w == providers.ProtocolResponses && strings.HasPrefix(models.ModelKey(wire), "grok") {
		if _, has := p.Route(providers.ProtocolOpenAI); !has {
			return providers.ProtocolOpenAI, false
		}
		return providers.ProtocolOpenAI, true
	}
	if !ok {
		return providers.ProtocolOpenAI, false
	}
	return w, true
}

// compatFor forces adaptive thinking on Claude ids Pi does not know: Databricks refuses thinking.type=enabled.
func compatFor(wire string) map[string]any {
	if models.FamilyKind(wire) == models.FamilyClaude && !strings.Contains(wire, "haiku-4") {
		return map[string]any{"forceAdaptiveThinking": true}
	}
	return nil
}

func modelEntry(p *providers.Provider, wire, label string) map[string]any {
	e := map[string]any{
		"id":        wire,
		"name":      label,
		"reasoning": models.FamilyKind(wire) == models.FamilyClaude || models.IsOpenAIReasoning(wire),
	}
	if l, ok := providers.LimitsFor(p, wire); ok {
		if l.Context > 0 {
			e["contextWindow"] = l.Context
		}
		if l.Output > 0 {
			e["maxTokens"] = l.Output
		}
	}
	return e
}

// BuildConfig renders Pi's models.json and settings.json for p's menu; menu maps each wire id to its Pi provider id.
func BuildConfig(p *providers.Provider, bases Bases) (modelsJSON, settingsJSON []byte, menu map[string]string, err error) {
	if p == nil {
		return nil, nil, nil, errors.New("pi: no provider")
	}
	entries, err := providers.OpencodeMenuFor(p)
	if err != nil {
		fmt.Fprintln(agentconfig.CatalogWarn, "pi:", err)
	}
	byWire := map[string][]map[string]any{}
	dropped := map[string][]string{}
	menu = map[string]string{}
	ids := map[string]string{}
	for _, pp := range piProviders {
		ids[pp.wire] = pp.id
	}
	for _, m := range entries {
		wire := agentconfig.MenuWire(p, m.ID)
		w, ok := pickWire(entries, p, wire)
		if !ok || bases.forWire(w) == "" {
			dropped[w] = append(dropped[w], m.Label)
			continue
		}
		e := modelEntry(p, wire, m.Label)
		if c := compatFor(wire); c != nil {
			e["compat"] = c
		}
		byWire[w] = append(byWire[w], e)
		menu[wire] = ids[w]
	}
	for _, pp := range piProviders {
		if d := dropped[pp.wire]; len(d) > 0 {
			fmt.Fprintf(agentconfig.CatalogWarn, "pi: %s has no %s route; left out: %s\n", p.Name, pp.wire, strings.Join(d, ", "))
		}
	}
	return render(p, bases, byWire, menu)
}

func render(p *providers.Provider, bases Bases, byWire map[string][]map[string]any, menu map[string]string) ([]byte, []byte, map[string]string, error) {
	out := map[string]any{}
	for _, pp := range piProviders {
		if len(byWire[pp.wire]) == 0 {
			continue
		}
		headers := maps.Clone(p.Headers)
		out[pp.id] = map[string]any{
			"baseUrl": bases.forWire(pp.wire), "api": pp.api, "apiKey": KeyRef, "headers": headers, "models": byWire[pp.wire],
		}
	}
	if len(out) == 0 {
		return nil, nil, nil, fmt.Errorf("pi: provider %s has no model on a routed wire", p.Name)
	}
	def := agentconfig.MenuWire(p, models.Strip1M(p.Models[models.Sonnet]))
	prov, ok := menu[def]
	if !ok {
		return nil, nil, nil, fmt.Errorf("pi: the %s sonnet alias %q is not on the Pi menu", p.Name, def)
	}
	mj, err := json.MarshalIndent(map[string]any{"providers": out}, "", " ")
	if err != nil {
		return nil, nil, nil, err
	}
	sj, err := json.MarshalIndent(map[string]any{"defaultProvider": prov, "defaultModel": def, "packages": []string{}}, "", " ")
	if err != nil {
		return nil, nil, nil, err
	}
	return mj, sj, menu, nil
}

// WriteConfig writes models.json and settings.json into dir (0700) with 0600 files, leaving unchanged ones alone.
func WriteConfig(dir string, modelsJSON, settingsJSON []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for name, data := range map[string][]byte{"models.json": modelsJSON, "settings.json": settingsJSON} {
		if _, err := paths.WriteIfChanged(filepath.Join(dir, name), data, 0o600); err != nil {
			return fmt.Errorf("pi: write %s: %w", name, err)
		}
	}
	return nil
}
