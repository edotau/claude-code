package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Each rejection is a distinct ParseAgents error, and none returns a partial map.
func TestParseAgentsRejections(t *testing.T) {
	cases := map[string]string{
		"empty":         " ",
		"roster key":    `{"codex":{"prompt":"p"}}`,
		"bad key":       `{"bad name":{"prompt":"p"}}`,
		"no prompt":     `{"sec":{"prompt":"  "}}`,
		"bad runner":    `{"sec":{"prompt":"p","runner":"nope"}}`,
		"unknown field": `{"sec":{"prompt":"p","modle":"x"}}`,
		"trailing data": `{"sec":{"prompt":"p"}} {}`,
		"missing file":  filepath.Join(t.TempDir(), "absent.json"),
		"not json":      `{"sec":`,
	}
	for name, arg := range cases {
		if got, err := ParseAgents(arg); err == nil || got != nil || !strings.HasPrefix(err.Error(), "--agents") {
			t.Errorf("%s: ParseAgents(%q) = %v, %v, want a --agents error and no map", name, arg, got, err)
		}
	}
}

// Inline JSON and a file of the same shape parse to the same stamped definition; "tools":[] stays non-nil.
func TestParseAgentsInlineAndFile(t *testing.T) {
	body := `{"sec":{"prompt":"Be strict.","runner":"codex","model":"m","profile":"p","effort":"high","max_turns":4,"tools":[]}}`
	file := filepath.Join(t.TempDir(), "agents.json")
	if err := os.WriteFile(file, []byte(body+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, arg := range []string{body, file} {
		got, err := ParseAgents(arg)
		d := got["sec"]
		if err != nil || d == nil || d.Name != "sec" || d.Runner != "codex" || d.Model != "m" || d.Profile != "p" ||
			d.Effort != "high" || d.MaxTurns != 4 || d.Tools == nil || len(d.Tools) != 0 {
			t.Errorf("ParseAgents(%.20q) = %+v, %v", arg, d, err)
		}
	}
}

// SelectAgent rejects an unknown vendor prefix and an unknown bare ref, and lets the vendor beat def.Runner.
func TestSelectAgentRejectionsAndVendorPrefix(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	agents := map[string]*Schema{"sec": {Name: "sec", Prompt: "p", Runner: "codex"}}
	for _, ref := range []string{"nope:sec", "ghost", "codex:ghost"} {
		if r, d, err := SelectAgent(ref, agents); err == nil {
			t.Errorf("SelectAgent(%q) = %q, %v, want an error", ref, r, d)
		}
	}
	if r, d, err := SelectAgent("sec", agents); err != nil || r != "codex" || d != agents["sec"] {
		t.Errorf("sec = %q, %v, %v, want codex with the definition", r, d, err)
	}
	if r, d, err := SelectAgent("gemini:sec", agents); err != nil || r != "gemini" || d != agents["sec"] {
		t.Errorf("gemini:sec = %q, %v, %v, want the vendor to beat def.Runner", r, d, err)
	}
	if r, d, err := SelectAgent("codex", agents); err != nil || r != "codex" || d != nil {
		t.Errorf("codex = %q, %v, %v, want a plain roster name", r, d, err)
	}
}

// Explicit Model/Provider/Effort beat the definition's; RunOptions.Agents beats Client.Agents on the same key.
func TestResolveOverlayAndAgentsPrecedence(t *testing.T) {
	c := &Client{Agents: map[string]*Schema{"sec": {Name: "sec", Prompt: "client", Model: "cm", Profile: "cp", Effort: "low"}}}
	o := RunOptions{Agent: "sec", Model: "mine", Provider: "myprov", Effort: "max"}
	if err := c.resolve(&o); err != nil || o.Model != "mine" || o.Provider != "myprov" || o.Effort != "max" || o.Agent != "claude" {
		t.Fatalf("explicit options lost: %+v, %v", o, err)
	}
	o = RunOptions{Agent: "sec"}
	if err := c.resolve(&o); err != nil || o.Model != "cm" || o.Provider != "cp" || o.Effort != "low" || o.Persona.Prompt != "client" {
		t.Fatalf("definition not overlaid: %+v, %v", o, err)
	}
	o = RunOptions{Agent: "sec", Agents: map[string]*Schema{"sec": {Name: "sec", Prompt: "run", Model: "rm"}}}
	if err := c.resolve(&o); err != nil || o.Persona.Prompt != "run" || o.Model != "rm" {
		t.Errorf("RunOptions.Agents must beat Client.Agents: %+v, %v", o, err)
	}
	if o = (RunOptions{Agent: "ghost"}); c.resolve(&o) == nil {
		t.Error("an unknown ref must fail resolve")
	}
}
