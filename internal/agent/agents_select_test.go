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

// Explicit Model/Provider/Effort beat the definition's.
func TestResolveOverlayAndAgentsPrecedence(t *testing.T) {
	c := &Client{}
	defs := map[string]*Schema{"sec": {Name: "sec", Prompt: "client", Model: "cm", Profile: "cp", Effort: "low"}}
	o := RunOptions{Agent: "sec", Agents: defs, Model: "mine", Provider: "myprov", Effort: "max"}
	if err := c.resolve(&o); err != nil || o.Model != "mine" || o.Provider != "myprov" || o.Effort != "max" || o.Agent != "claude" {
		t.Fatalf("explicit options lost: %+v, %v", o, err)
	}
	o = RunOptions{Agent: "sec", Agents: defs}
	if err := c.resolve(&o); err != nil || o.Model != "cm" || o.Provider != "cp" || o.Effort != "low" || o.Persona.Prompt != "client" {
		t.Fatalf("definition not overlaid: %+v, %v", o, err)
	}
	if o = (RunOptions{Agent: "ghost"}); c.resolve(&o) == nil {
		t.Error("an unknown ref must fail resolve")
	}
}

// A definition with `model: inherit` carries no model of its own, so nothing reaches routing.
func TestOverlayInheritModelIsNone(t *testing.T) {
	c := &Client{}
	o := RunOptions{Agent: "plan", Agents: map[string]*Schema{"plan": {Name: "plan", Prompt: "p", Model: "inherit"}}}
	if err := c.resolve(&o); err != nil || o.Model != "" || o.Persona.Model != "inherit" {
		t.Fatalf("inherit leaked into the request model: %+v, %v", o, err)
	}
}

// Record resolves the selector to its runner but, unlike Prepare, never routes or applies the rubric.
func TestClientRecordSkipsRouting(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	defs, err := ParseAgents(`{"sec":{"prompt":"p","runner":"codex"}}`)
	if err != nil {
		t.Fatal(err)
	}
	c := NewClient(nil, nil, nil)
	o := RunOptions{Agent: "sec", Agents: defs, Prompt: "x", Provider: "no-such-provider", Rubric: "/nonexistent/rubric.md"}
	if _, err := c.Prepare(t.Context(), o, nil); err == nil {
		t.Fatal("control: Prepare should fail on the unknown provider")
	}
	rec, err := c.Record(t.Context(), o, nil)
	if err != nil || rec.Agent != "codex" {
		t.Fatalf("Record = agent %q, err %v; want codex and no error", rec.Agent, err)
	}
}
