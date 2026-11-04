package agent

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/textutil"
)

var personaNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func personaName(p *Schema) string {
	if p == nil {
		return ""
	}
	return p.Name
}

// LoadSchema resolves ref: a bare name is <config dir>/agents/<name>.md, anything else is a file path.
func LoadSchema(ref string) (*Schema, error) {
	path, err := schemaPath(ref)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("persona %q: %w", ref, err)
	}
	fm, body := textutil.ParseFrontmatter(string(raw))
	str := func(k string) string { s, _ := fm[k].(string); return strings.TrimSpace(s) }
	p := &Schema{Name: str("name"), Description: str("description"), Prompt: body, Model: str("model"),
		Agent: str("agent"), Profile: str("profile"), Effort: str("effort")}
	if _, old := fm["runner"]; old {
		return nil, fmt.Errorf("persona %q: frontmatter key runner is now agent", ref)
	}
	if p.Name == "" {
		p.Name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	if !personaNameRE.MatchString(p.Name) {
		return nil, fmt.Errorf("persona %q: name %q is not a valid agent name", ref, p.Name)
	}
	if strings.TrimSpace(p.Prompt) == "" {
		return nil, fmt.Errorf("persona %q: %s has no body to use as the prompt", ref, path)
	}
	p.Tools = schemaTools(fm["tools"])
	if n, ok := fm["maxTurns"].(int); ok && n > 0 {
		p.MaxTurns = n
	}
	return p, nil
}

func schemaPath(ref string) (string, error) {
	if strings.ContainsAny(ref, `/\`) || strings.HasSuffix(ref, ".md") {
		return ref, nil
	}
	if !personaNameRE.MatchString(ref) {
		return "", fmt.Errorf("persona %q: not a valid agent name", ref)
	}
	return filepath.Join(paths.ConfigDir(), "agents", ref+".md"), nil
}

func schemaTools(tools any) []string {
	switch t := tools.(type) {
	case []string:
		return append([]string{}, t...)
	case string:
		out := []string{}
		for f := range strings.SplitSeq(t, ",") {
			if f = strings.TrimSpace(f); f != "" {
				out = append(out, f)
			}
		}
		return out
	}
	return nil
}

// agentsDef is the strict wire shape of one --agents entry; every key is optional except the prompt.
type agentsDef struct {
	Description string   `json:"description"`
	Prompt      string   `json:"prompt"`
	Tools       []string `json:"tools"` // nil = inherit, empty = no tools
	Model       string   `json:"model"`
	MaxTurns    int      `json:"max_turns"`
	Agent       string   `json:"agent"`
	Profile     string   `json:"profile"`
	Effort      string   `json:"effort"`
}

// ParseAgents reads an --agents argument: inline JSON when it starts with "{", else a file of the same shape.
func ParseAgents(arg string) (map[string]*Schema, error) {
	body := strings.TrimSpace(arg)
	if body == "" {
		return nil, errors.New("--agents: no definitions")
	}
	if !strings.HasPrefix(body, "{") {
		data, err := os.ReadFile(arg)
		if err != nil {
			return nil, fmt.Errorf("--agents: %w", err)
		}
		body = strings.TrimSpace(string(data))
	}
	dec := json.NewDecoder(strings.NewReader(body))
	dec.DisallowUnknownFields()
	var wire map[string]agentsDef
	if err := dec.Decode(&wire); err != nil {
		if strings.Contains(err.Error(), `unknown field "runner"`) {
			return nil, errors.New(`--agents: key "runner" is now "agent"`)
		}
		return nil, fmt.Errorf("--agents: %w", err)
	}
	if extra := (json.RawMessage)(nil); !errors.Is(dec.Decode(&extra), io.EOF) {
		return nil, errors.New("--agents: trailing data after the JSON object")
	}
	return buildAgents(wire)
}

// buildAgents validates the decoded entries in key order and stamps each Schema.Name with its key.
func buildAgents(wire map[string]agentsDef) (map[string]*Schema, error) {
	names := rosterNames()
	agents := make(map[string]*Schema, len(wire))
	for _, key := range slices.Sorted(maps.Keys(wire)) {
		d := wire[key]
		switch {
		case slices.Contains(names, key):
			return nil, fmt.Errorf("--agents: key %q is a roster agent name (%s)", key, strings.Join(names, "|"))
		case !personaNameRE.MatchString(key):
			return nil, fmt.Errorf("--agents: key %q is not a valid agent name", key)
		case strings.TrimSpace(d.Prompt) == "":
			return nil, fmt.Errorf("--agents: %s has no prompt", key)
		case d.Agent != "" && !slices.Contains(names, d.Agent):
			return nil, fmt.Errorf("--agents: %s agent %q is not a roster agent (%s)", key, d.Agent, strings.Join(names, "|"))
		}
		agents[key] = &Schema{Name: key, Description: d.Description, Prompt: d.Prompt, Tools: d.Tools, Model: d.Model,
			MaxTurns: d.MaxTurns, Agent: d.Agent, Profile: d.Profile, Effort: d.Effort}
	}
	return agents, nil
}

// rosterNames lists the roster agents' names in roster order.
func rosterNames() []string {
	var names []string
	for _, s := range Roster() {
		names = append(names, s.Name)
	}
	return names
}

// SelectAgent resolves an --agent value: a roster name, an agents key, vendor:name, or agents/<name>.md or a file.
func SelectAgent(ref string, agents map[string]*Schema) (agent string, def *Schema, err error) {
	if _, ok := LookupSchema(ref); ok {
		return ref, nil, nil
	}
	names := rosterNames()
	if def, ok := agents[ref]; ok {
		return cmp.Or(def.Agent, "claude"), def, nil
	}
	if v, name, ok := strings.Cut(ref, ":"); ok {
		if !slices.Contains(names, v) {
			return "", nil, fmt.Errorf("unknown agent %q (%s)", v, strings.Join(names, "|"))
		}
		if def, ok := agents[name]; ok { // the agent beats def.Agent
			return v, def, nil
		}
		def, err = LoadSchema(name)
		return v, def, err
	}
	if def, err = LoadSchema(ref); err != nil {
		return "", nil, fmt.Errorf("unknown agent %q (%s, or an agents/<name>.md definition): %w", ref, strings.Join(names, "|"), err)
	}
	return cmp.Or(def.Agent, "claude"), def, nil
}

// RefusePersona rejects a persona whose tool limit an emulating agent could not enforce.
func RefusePersona(c Caps, agentName string, p *Schema) error {
	if p == nil || c.Persona != Emulated || p.Tools == nil {
		return nil
	}
	for _, t := range []string{"Bash", "Write", "Edit"} {
		if !slices.Contains(p.Tools, t) {
			return fmt.Errorf("persona %q limits tools to %q and %s cannot enforce that; run it on claude", p.Name, strings.Join(p.Tools, ","), agentName)
		}
	}
	return nil
}

// personaArgs renders claude's `--agents '{"<name>":{…}}' --agent <name>` with only the selected persona.
func personaArgs(p *Schema) []string {
	def := map[string]any{"description": cmp.Or(p.Description, p.Name), "prompt": p.Prompt}
	if p.Tools != nil {
		def["tools"] = p.Tools
	}
	if p.Model != "" {
		def["model"] = p.Model
	}
	if p.MaxTurns > 0 {
		def["maxTurns"] = p.MaxTurns
	}
	b, _ := json.Marshal(map[string]any{p.Name: def})
	return []string{"--agents", string(b), "--agent", p.Name}
}

// emulatePersona folds the persona into system text for an agent with no definition channel and names the fields lost.
func emulatePersona(c Caps, req RunOptions, dropped []string) (RunOptions, []string) {
	p := req.Persona
	if p.Tools != nil {
		dropped = append(dropped, "persona.tools="+cmp.Or(strings.Join(p.Tools, ","), "none"))
	}
	if p.MaxTurns > 0 {
		dropped = append(dropped, fmt.Sprintf("persona.max_turns=%d", p.MaxTurns))
	}
	text := "You are the " + p.Name + " agent."
	if p.Description != "" {
		text += " " + p.Description
	}
	text += "\n\n" + p.Prompt
	req.Persona = nil
	if c.AppendSystemPrompt == Emulated {
		req.Prompt = Preamble(text) + req.Prompt
	} else {
		req.AppendSystemPrompt = strings.TrimSpace(text + "\n\n" + req.AppendSystemPrompt)
	}
	return req, dropped
}
