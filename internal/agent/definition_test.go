package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestLoadSchemaUnknown(t *testing.T) {
	setConfigDir(t)
	for _, ref := range []string{"missing", "../x", "/no/such.md", "bad name"} {
		if p, err := loadSchema(ref); err == nil {
			t.Errorf("LoadSchema(%q) = %+v, want an error", ref, p)
		}
	}
}

// An explicit empty tools list means no tools: it survives load, the claude definition and the record round-trip.
func TestPersonaEmptyToolsIsNotInherit(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	f := filepath.Join(dir, "r.md")
	if err := os.WriteFile(f, []byte("---\nname: r\ntools: []\n---\nBody.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := loadSchema(f)
	if err != nil || p.Tools == nil || len(p.Tools) != 0 {
		t.Fatalf("loaded %+v (%v), want non-nil empty Tools", p, err)
	}
	if args := PersonaArgs(p); !strings.Contains(args[1], `"tools":[]`) {
		t.Errorf("--agents = %s, want tools:[]", args[1])
	}
	b, _ := json.Marshal(RunOptions{Persona: p})
	var back RunOptions
	if err := json.Unmarshal(b, &back); err != nil || back.Persona.Tools == nil {
		t.Errorf("round-trip lost the empty list: %s (%v)", b, err)
	}
	if _, dropped, _ := applyCaps(Caps{Persona: Emulated, AppendSystemPrompt: Emulated}, RunOptions{Persona: p}); !slices.Contains(dropped, "persona.tools=none") {
		t.Errorf("dropped = %q, want tools=none named", dropped)
	}
}

func TestSelectAgent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	if err := os.MkdirAll(filepath.Join(dir, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"codex", "helper"} {
		if err := os.WriteFile(filepath.Join(dir, "agents", n+".md"), []byte("---\nname: "+n+"\n---\nBody "+n+".\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(dir, "x.md")
	if err := os.WriteFile(file, []byte("Body x.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	agents := map[string]*Schema{
		"sec":   {Name: "sec", Prompt: "S.", Agent: "codex"},
		"plain": {Name: "plain", Prompt: "P."},
		"help":  {Name: "help", Prompt: "H.", Agent: "gemini"},
	}
	for _, c := range []struct{ ref, agent, persona string }{
		{"codex", "codex", ""},              // the roster name beats agents/codex.md
		{"sec", "codex", "sec"},             // an agents key resolves to def.Agent
		{"plain", "claude", "plain"},        // an agents key with no agent runs on claude
		{"codex:helper", "codex", "helper"}, // vendor:name loads agents/<name>.md
		{"codex:help", "codex", "help"},     // the vendor beats def.Agent
		{"helper", "claude", "helper"},      // agents/<name>.md with no agent runs on claude
		{file, "claude", "x"},               // a file path
	} {
		v, p, err := SelectAgent(c.ref, agents)
		if err != nil || v != c.agent || personaName(p) != c.persona {
			t.Errorf("SelectAgent(%q) = %q, %+v, %v; want %q persona %q", c.ref, v, p, err, c.agent, c.persona)
		}
	}
	assertSelectAgentUnknownRefs(t)
}

func assertSelectAgentUnknownRefs(t *testing.T) {
	t.Helper()
	for _, ref := range []string{"notvendor:helper", "nosuch"} {
		if _, _, err := SelectAgent(ref, nil); err == nil || !strings.Contains(err.Error(), "codex") {
			t.Errorf("SelectAgent(%q) err = %v, want one listing the vendors", ref, err)
		}
	}
}

func TestRefusePersona(t *testing.T) {
	emu, nat := Caps{Persona: Emulated}, Caps{Persona: Native}
	p := func(tools []string) *Schema { return &Schema{Name: "p", Tools: tools} }
	for _, c := range []struct {
		name   string
		caps   Caps
		tools  []string
		refuse bool
	}{
		{"nil tools inherit", emu, nil, false},
		{"full toolset", emu, []string{"Bash", "Write", "Edit", "Read"}, false},
		{"scoped Bash is not Bash", emu, []string{"Bash(go test*)", "Write", "Edit"}, true},
		{"empty means no tools", emu, []string{}, true},
		{"read only", emu, []string{"Read"}, true},
		{"native never refuses", nat, []string{}, false},
	} {
		if err := refusePersona(c.caps, "codex", p(c.tools)); (err != nil) != c.refuse {
			t.Errorf("%s: err = %v, refuse want %v", c.name, err, c.refuse)
		}
	}
	if err := refusePersona(emu, "codex", nil); err != nil {
		t.Errorf("nil persona: %v", err)
	}
}

// Each rejection is a distinct ParseAgents error, and none returns a partial map.
func TestParseAgentsRejections(t *testing.T) {
	cases := map[string]string{
		"empty":         " ",
		"roster key":    `{"codex":{"prompt":"p"}}`,
		"bad key":       `{"bad name":{"prompt":"p"}}`,
		"no prompt":     `{"sec":{"prompt":"  "}}`,
		"bad agent":     `{"sec":{"prompt":"p","agent":"nope"}}`,
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

// The retired "runner" key yields the one-line rename hint, not the raw decoder error.
func TestParseAgentsOldRunnerKeyHint(t *testing.T) {
	_, err := ParseAgents(`{"sec":{"prompt":"p","runner":"codex"}}`)
	if want := `--agents: key "runner" is now "agent"`; err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
}

// Inline JSON and a file of the same shape parse to the same stamped definition; "tools":[] stays non-nil.
func TestParseAgentsInlineAndFile(t *testing.T) {
	body := `{"sec":{"prompt":"Be strict.","agent":"codex","model":"m","profile":"p","effort":"high","max_turns":4,"tools":[]}}`
	file := filepath.Join(t.TempDir(), "agents.json")
	if err := os.WriteFile(file, []byte(body+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, arg := range []string{body, file} {
		got, err := ParseAgents(arg)
		assertParsedAgentDefinition(t, arg, got["sec"], err)
	}
}

func assertParsedAgentDefinition(t *testing.T, arg string, d *Schema, err error) {
	t.Helper()
	if err != nil || d == nil {
		t.Errorf("ParseAgents(%.20q) = %+v, %v", arg, d, err)
		return
	}
	assertParsedAgentFields(t, arg, d, err)
}

func assertParsedAgentFields(t *testing.T, arg string, d *Schema, err error) {
	t.Helper()
	if d.Name != "sec" || d.Agent != "codex" || d.Model != "m" || d.Profile != "p" ||
		d.Effort != "high" || d.MaxTurns != 4 || d.Tools == nil || len(d.Tools) != 0 {
		t.Errorf("ParseAgents(%.20q) = %+v, %v", arg, d, err)
	}
}

// SelectAgent rejects an unknown vendor prefix and an unknown bare ref, and lets the vendor beat def.Agent.
func TestSelectAgentRejectionsAndVendorPrefix(t *testing.T) {
	setConfigDir(t)
	agents := map[string]*Schema{"sec": {Name: "sec", Prompt: "p", Agent: "codex"}}
	for _, ref := range []string{"nope:sec", "ghost", "codex:ghost"} {
		if r, d, err := SelectAgent(ref, agents); err == nil {
			t.Errorf("SelectAgent(%q) = %q, %v, want an error", ref, r, d)
		}
	}
	assertSelectAgentVendorPrecedence(t, agents)
}

func assertSelectAgentVendorPrecedence(t *testing.T, agents map[string]*Schema) {
	t.Helper()
	if r, d, err := SelectAgent("sec", agents); err != nil || r != "codex" || d != agents["sec"] {
		t.Errorf("sec = %q, %v, %v, want codex with the definition", r, d, err)
	}
	if r, d, err := SelectAgent("gemini:sec", agents); err != nil || r != "gemini" || d != agents["sec"] {
		t.Errorf("gemini:sec = %q, %v, %v, want the vendor to beat def.Agent", r, d, err)
	}
	if r, d, err := SelectAgent("codex", agents); err != nil || r != "codex" || d != nil {
		t.Errorf("codex = %q, %v, %v, want a plain roster name", r, d, err)
	}
}

func TestApplyCaps(t *testing.T) {
	req := RunOptions{Prompt: "p", Model: "m", Provider: "openai", Effort: "high", WorkDir: "/tmp", SessionID: "s", Tools: true}
	got, dropped, _ := applyCaps(Caps{Model: Native, Provider: Native}, req)
	want := []string{"--effort=high", "--workdir=/tmp", "--session=s", "--tools"}
	if !reflect.DeepEqual(dropped, want) {
		t.Errorf("dropped = %q, want %q", dropped, want)
	}
	if got.Effort != "" || got.WorkDir != "" || got.SessionID != "" || got.Tools || got.Model != "m" || got.Provider != "openai" {
		t.Errorf("request not cleared to caps: %+v", got)
	}
	if _, d, _ := applyCaps(Caps{}, RunOptions{Prompt: "p"}); d != nil {
		t.Errorf("unset fields must not be reported, got %q", d)
	}
}

type capsWant struct{ sys, app, schema Level }

// wantEmulation derives the emulated flags and the prompt applyCaps must produce for the wanted levels.
func wantEmulation(w capsWant, schema string) ([]string, string) {
	var wantEm []string
	wantPrompt := "TASK"
	if w.sys == Emulated {
		wantEm = append(wantEm, "--system-prompt")
		wantPrompt = Preamble("SYS") + wantPrompt
	}
	if w.app == Emulated {
		wantEm = append(wantEm, "--append-system-prompt")
		wantPrompt = Preamble("APP") + wantPrompt
	}
	if w.schema != Native {
		wantEm = append(wantEm, "--output-schema")
		wantPrompt += "\n\nAnswer with only a JSON value matching this JSON Schema, no prose or code fence: " + `{"type":"object"}`
	}
	return wantEm, wantPrompt
}

func assertAppliedCaps(t *testing.T, c Caps, w capsWant, schema string) {
	t.Helper()
	in := RunOptions{Prompt: "TASK", SystemPrompt: "SYS", AppendSystemPrompt: "APP", OutputSchema: schema, Env: []string{"A=1"}}
	out, dropped, emulated := applyCaps(c, in)
	if len(dropped) != 0 || !slices.Equal(out.Env, in.Env) {
		t.Errorf("dropped = %v, env = %v; env is native everywhere", dropped, out.Env)
	}
	wantEm, wantPrompt := wantEmulation(w, schema)
	if !slices.Equal(emulated, wantEm) {
		t.Errorf("emulated = %v, want %v", emulated, wantEm)
	}
	if out.Prompt != wantPrompt {
		t.Errorf("prompt = %q, want %q", out.Prompt, wantPrompt)
	}
	// a Native flag stays on the request for the adapter's own flag; an Emulated one is consumed
	if (out.SystemPrompt != "") != (w.sys == Native) || (out.AppendSystemPrompt != "") != (w.app == Native) {
		t.Errorf("request sys=%q append=%q after applyCaps", out.SystemPrompt, out.AppendSystemPrompt)
	}
	if out.OutputSchema != schema {
		t.Errorf("OutputSchema must survive for the validator, got %q", out.OutputSchema)
	}
}

// The capability matrix end to end: each agent's real Caps through applyCaps, per level.
func TestCapabilityMatrixApplyCaps(t *testing.T) {
	const schema = `{"type": "object"}`
	want := map[string]capsWant{
		"claude":   {Native, Native, Native},
		"codex":    {Native, Native, Native},
		"gemini":   {Emulated, Emulated, Native},
		"opencode": {Native, Emulated, Emulated},
		"copilot":  {Native, Native, Emulated},
	}
	for _, r := range Engines() {
		w, ok := want[r.Name()]
		if !ok {
			t.Fatalf("unexpected agent %q", r.Name())
		}
		t.Run(r.Name(), func(t *testing.T) {
			c := r.Caps()
			if c.SystemPrompt != w.sys || c.AppendSystemPrompt != w.app || c.OutputSchema != w.schema || c.Env != Native {
				t.Fatalf("caps sys=%v append=%v schema=%v env=%v, want %v/%v/%v/native", c.SystemPrompt, c.AppendSystemPrompt, c.OutputSchema, c.Env, w.sys, w.app, w.schema)
			}
			assertAppliedCaps(t, c, w, schema)
		})
	}
}

// Level None drops and names the flag; it is neither emulated nor passed on.
func TestApplyCapsNoneDropsSystemPrompt(t *testing.T) {
	out, dropped, emulated := applyCaps(Caps{}, RunOptions{Prompt: "T", SystemPrompt: "S", AppendSystemPrompt: "A"})
	if !slices.Equal(dropped, []string{"--system-prompt", "--append-system-prompt"}) || len(emulated) != 0 || out.Prompt != "T" ||
		out.SystemPrompt != "" || out.AppendSystemPrompt != "" {
		t.Errorf("out=%+v dropped=%v emulated=%v", out, dropped, emulated)
	}
}
