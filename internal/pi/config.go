package pi

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/schemas"
)

var _ = schemas.Register(schemas.Format{
	Name: "pi-config", Owner: "internal/pi", Kind: schemas.Derived,
	Glob:    func() string { return filepath.Join(paths.StateDir(), "pi", "*", "runs", "*", "*.json") },
	Version: 1, Clean: "pi-runs",
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

// RunDirPrefix starts every run dir's name; the owning harness pid follows (run-<pid>-<random>), so `clean` can tell a live run.
const RunDirPrefix = "run-"

// RunsDir is the parent of every per-dispatch agent dir of profile; sessions live outside it, in pi/sessions.
func RunsDir(profile string) string {
	return filepath.Join(paths.StateDir(), "pi", profile, "runs")
}

// NewRunDir makes a private (0700) agent dir for one dispatch, so parallel dispatches never share a models.json.
func NewRunDir(profile string) (dir string, remove func(), err error) {
	root := RunsDir(profile)
	if err = os.MkdirAll(root, 0o700); err != nil {
		return "", nil, err
	}
	if dir, err = os.MkdirTemp(root, RunDirPrefix+strconv.Itoa(os.Getpid())+"-"); err != nil {
		return "", nil, err
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

// pickWire is the Pi wire for a model: Responses where p serves it, else chat; Claude keeps anthropic.
// Grok accepts Responses but Databricks does not stream it, so it goes to chat.
func pickWire(p *providers.Provider, wire string) (string, bool) {
	has := func(d string) bool { _, ok := p.Route(d); return ok }
	switch {
	case models.FamilyKind(wire) == models.FamilyClaude && has(providers.ProtocolAnthropic):
		return providers.ProtocolAnthropic, true
	case models.FamilyKind(wire) != models.FamilyClaude && !strings.HasPrefix(models.ModelKey(wire), "grok") &&
		providers.ResponsesServable(p, wire) && has(providers.ProtocolResponses):
		return providers.ProtocolResponses, true
	}
	return providers.ProtocolOpenAI, has(providers.ProtocolOpenAI)
}

// compatFor is Pi's per-model compat: Databricks refuses thinking.type=enabled on every Claude id but haiku-4*.
func compatFor(wire string) map[string]any {
	if models.FamilyKind(wire) == models.FamilyClaude && !strings.Contains(wire, "haiku-4") {
		return map[string]any{"forceAdaptiveThinking": true}
	}
	return nil
}

// chatCompat is the dbx-chat provider's compat: Databricks' chat route rejects the store parameter (HTTP 400).
var chatCompat = map[string]any{"supportsStore": false}

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
// The default model is want, else the first menu entry, so the run's own model never depends on the sonnet alias.
func BuildConfig(p *providers.Provider, bases Bases, want string) (modelsJSON, settingsJSON []byte, menu map[string]string, err error) {
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
	first := ""
	ids := map[string]string{}
	for _, pp := range piProviders {
		ids[pp.wire] = pp.id
	}
	for _, m := range entries {
		wire := agentconfig.MenuWire(p, m.ID)
		w, ok := pickWire(p, wire)
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
		first = cmp.Or(first, wire)
	}
	for _, pp := range piProviders {
		if d := dropped[pp.wire]; len(d) > 0 {
			fmt.Fprintf(agentconfig.CatalogWarn, "pi: %s has no %s route; left out: %s\n", p.Name, pp.wire, strings.Join(d, ", "))
		}
	}
	return render(p, bases, byWire, menu, cmp.Or(onMenu(menu, want), first))
}

func onMenu(menu map[string]string, id string) string {
	if _, ok := menu[id]; ok {
		return id
	}
	return ""
}

func render(p *providers.Provider, bases Bases, byWire map[string][]map[string]any, menu map[string]string, def string) ([]byte, []byte, map[string]string, error) {
	out := map[string]any{}
	for _, pp := range piProviders {
		if len(byWire[pp.wire]) == 0 {
			continue
		}
		headers := maps.Clone(p.Headers)
		prov := map[string]any{
			"baseUrl": bases.forWire(pp.wire), "api": pp.api, "apiKey": KeyRef, "headers": headers, "models": byWire[pp.wire],
		}
		if pp.wire == providers.ProtocolOpenAI {
			prov["compat"] = chatCompat
		}
		out[pp.id] = prov
	}
	if len(out) == 0 {
		return nil, nil, nil, fmt.Errorf("pi: provider %s has no model on a routed wire", p.Name)
	}
	mj, err := json.MarshalIndent(map[string]any{"providers": out}, "", " ")
	if err != nil {
		return nil, nil, nil, err
	}
	sj, err := json.MarshalIndent(map[string]any{"defaultProvider": menu[def], "defaultModel": def, "packages": []string{}}, "", " ")
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
