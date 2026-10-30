package agent

import (
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/textutil"
)

// Persona is an inline agent definition: claude takes it as --agents/--agent, the rest get it as system text.
type Persona struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Prompt      string   `json:"prompt"`
	Tools       []string `json:"tools"` // nil = inherit, empty = no tools
	Model       string   `json:"model,omitempty"`
	MaxTurns    int      `json:"max_turns,omitempty"`
}

var personaNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func personaName(p *Persona) string {
	if p == nil {
		return ""
	}
	return p.Name
}

// LoadPersona resolves ref: a bare name is <config dir>/agents/<name>.md, anything else is a file path.
func LoadPersona(ref string) (*Persona, error) {
	path := ref
	if !strings.ContainsAny(ref, `/\`) && !strings.HasSuffix(ref, ".md") {
		if !personaNameRE.MatchString(ref) {
			return nil, fmt.Errorf("persona %q: not a valid agent name", ref)
		}
		path = filepath.Join(paths.ConfigDir(), "agents", ref+".md")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("persona %q: %w", ref, err)
	}
	fm, body := textutil.ParseFrontmatter(string(raw))
	str := func(k string) string { s, _ := fm[k].(string); return strings.TrimSpace(s) }
	p := &Persona{Name: str("name"), Description: str("description"), Prompt: body, Model: str("model")}
	if p.Name == "" {
		p.Name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	if !personaNameRE.MatchString(p.Name) {
		return nil, fmt.Errorf("persona %q: name %q is not a valid agent name", ref, p.Name)
	}
	if strings.TrimSpace(p.Prompt) == "" {
		return nil, fmt.Errorf("persona %q: %s has no body to use as the prompt", ref, path)
	}
	switch t := fm["tools"].(type) {
	case []string:
		p.Tools = append([]string{}, t...)
	case string:
		p.Tools = []string{}
		for f := range strings.SplitSeq(t, ",") {
			if f = strings.TrimSpace(f); f != "" {
				p.Tools = append(p.Tools, f)
			}
		}
	}
	if n, ok := fm["maxTurns"].(int); ok && n > 0 {
		p.MaxTurns = n
	}
	return p, nil
}

// ResolveTarget splits an --agent value: a vendor name, vendor:persona, or an agents/<name>.md name or file on claude.
func ResolveTarget(ref string) (vendor string, p *Persona, err error) {
	var names []string
	for _, r := range Runners() {
		names = append(names, r.Name())
	}
	if slices.Contains(names, ref) {
		return ref, nil, nil
	}
	if v, name, ok := strings.Cut(ref, ":"); ok {
		if !slices.Contains(names, v) {
			return "", nil, fmt.Errorf("unknown agent %q (%s)", v, strings.Join(names, "|"))
		}
		p, err = LoadPersona(name)
		return v, p, err
	}
	if p, err = LoadPersona(ref); err != nil {
		return "", nil, fmt.Errorf("unknown agent %q (%s, or an agents/<name>.md definition): %w", ref, strings.Join(names, "|"), err)
	}
	return "claude", p, nil
}

// RefusePersona rejects a persona whose tool limit an emulating agent could not enforce.
func RefusePersona(c Caps, agentName string, p *Persona) error {
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
func personaArgs(p Persona) []string {
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
func emulatePersona(c Caps, req Request, dropped []string) (Request, []string) {
	p := req.Persona
	if p.Tools != nil {
		dropped = append(dropped, "persona.tools="+cmp.Or(strings.Join(p.Tools, ","), "none"))
	}
	if p.Model != "" {
		dropped = append(dropped, "persona.model="+p.Model)
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
		req.Prompt = preamble(text) + req.Prompt
	} else {
		req.AppendSystemPrompt = strings.TrimSpace(text + "\n\n" + req.AppendSystemPrompt)
	}
	return req, dropped
}
