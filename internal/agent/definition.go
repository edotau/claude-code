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

	"github.com/edotau/claude-code/internal/agent/wire"
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

// loadSchema resolves ref: a bare name is <config dir>/agents/<name>.md, anything else is a file path.
func loadSchema(ref string) (*Schema, error) {
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
	if p.Tools, err = schemaTools(fm["tools"]); err != nil {
		return nil, fmt.Errorf("persona %q: %w", ref, err)
	}
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

// schemaTools reads a tools value: absent inherits (nil), a list or comma string restricts; anything else is an error.
func schemaTools(tools any) ([]string, error) {
	switch t := tools.(type) {
	case nil:
		return nil, nil
	case []string:
		return append([]string{}, t...), nil
	case string:
		out := []string{}
		for f := range strings.SplitSeq(t, ",") {
			if f = strings.TrimSpace(f); f != "" {
				out = append(out, f)
			}
		}
		return out, nil
	}
	return nil, fmt.Errorf("tools must be a list or comma-separated string, not %v", tools)
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
	if wire == nil { // a top-level null decodes to a nil map without error
		return nil, errors.New("--agents: top-level value must be a JSON object, not null")
	}
	names := RosterNames()
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

// SelectAgent resolves an --agent value: a roster name, an agents key, vendor:name, or agents/<name>.md or a file.
func SelectAgent(ref string, agents map[string]*Schema) (agent string, def *Schema, err error) {
	if _, ok := LookupSchema(ref); ok {
		return ref, nil, nil
	}
	names := RosterNames()
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
		def, err = loadSchema(name)
		return v, def, err
	}
	if def, err = loadSchema(ref); err != nil {
		return "", nil, unknownPersonaError(ref, names, err)
	}
	return cmp.Or(def.Agent, "claude"), def, nil
}

func unknownPersonaError(ref string, names []string, err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("unknown agent %q (%s, or an agents/<name>.md definition)", ref, strings.Join(names, "|"))
	}
	return fmt.Errorf("unknown agent %q (%s, or an agents/<name>.md definition): %w", ref, strings.Join(names, "|"), err)
}

// refusePersona rejects a persona whose tool limit an emulating agent could not enforce.
func refusePersona(c Caps, agentName string, p *Schema) error {
	if p == nil || c.Persona != Emulated || p.Tools == nil {
		return nil
	}
	for _, r := range ReadOnlyDeny {
		if !slices.Contains(p.Tools, r.Tool) {
			return fmt.Errorf("persona %q limits tools to %q and %s cannot enforce that; run it on claude", p.Name, strings.Join(p.Tools, ","), agentName)
		}
	}
	return nil
}

// PersonaArgs renders claude's `--agents '{"<name>":{…}}' --agent <name>` with only the selected persona.
func PersonaArgs(p *Schema) []string {
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

// applyCaps clears fields the agent cannot honor and names each drop; Emulated flags run a harness shim here.
func applyCaps(c Caps, req RunOptions) (out RunOptions, dropped, emulated []string) {
	drop := func(l Level, set bool, flag string, clear func()) {
		if set && l == None {
			dropped = append(dropped, flag)
			clear()
		}
	}
	drop(c.Model, req.Model != "", "--model="+req.Model, func() { req.Model = "" })
	drop(c.Provider, req.Provider != "", "--provider="+req.Provider, func() { req.Provider = "" })
	drop(c.Effort, req.Effort != "", "--effort="+req.Effort, func() { req.Effort = "" })
	drop(c.WorkDir, req.WorkDir != "", "--workdir="+req.WorkDir, func() { req.WorkDir = "" })
	drop(c.SessionID, req.SessionID != "", "--session="+req.SessionID, func() { req.SessionID = "" })
	drop(c.ReadOnly, req.ReadOnly, "--read-only (tools off)", func() { req.ReadOnly, req.Tools = false, false }) // fail closed
	drop(c.Tools, req.Tools, "--tools", func() { req.Tools = false })
	drop(c.NoTools, req.NoTools, "--no-tools", func() { req.NoTools = false })
	drop(c.Autonomous, req.Autonomous, "--autonomous", func() { req.Autonomous = false })
	drop(c.Fleet, req.Fleet, "--fleet", func() { req.Fleet = false })
	drop(c.Memory, req.EnableMemory, "--enable-memory", func() { req.EnableMemory = false })
	drop(c.NoPersist, req.NoPersist, "--no-persist", func() { req.NoPersist = false })
	drop(c.SafeMode, req.SafeMode, "--safe-mode", func() { req.SafeMode = false })
	drop(c.Direct, req.Direct, "--direct", func() { req.Direct = false })
	drop(c.DebugFile, req.DebugFile != "", "--debug-file="+req.DebugFile, func() { req.DebugFile = "" })
	drop(c.Env, len(req.Env) > 0, "--env", func() { req.Env = nil })
	drop(c.Permissions, !req.Permissions.IsZero(), "--allow/--ask/--mode", func() { req.Permissions = Permissions{} })
	drop(c.Persona, req.Persona != nil, "persona="+personaName(req.Persona), func() { req.Persona = nil })
	if req.Persona != nil && c.Persona == Emulated {
		req, dropped = emulatePersona(c, req, dropped)
		emulated = append(emulated, "persona")
	}
	for _, sp := range []struct {
		field *string
		level Level
		flag  string
	}{{&req.SystemPrompt, c.SystemPrompt, "--system-prompt"}, {&req.AppendSystemPrompt, c.AppendSystemPrompt, "--append-system-prompt"}} {
		if *sp.field != "" && sp.level == Emulated {
			req.Prompt, *sp.field = Preamble(*sp.field)+req.Prompt, ""
			emulated = append(emulated, sp.flag)
		}
	}
	drop(c.SystemPrompt, req.SystemPrompt != "", "--system-prompt", func() { req.SystemPrompt = "" })
	drop(c.AppendSystemPrompt, req.AppendSystemPrompt != "", "--append-system-prompt", func() { req.AppendSystemPrompt = "" })
	if req.OutputSchema != "" && c.OutputSchema != Native { // the answer is validated either way; only the instruction differs
		req.Prompt += wire.SchemaInstruction(req.OutputSchema)
		emulated = append(emulated, "--output-schema")
	}
	return req, dropped, emulated
}
