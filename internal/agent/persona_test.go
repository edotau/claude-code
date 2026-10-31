package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var testPersona = &Schema{Name: "reviewer", Description: "Reviews", Prompt: "Be strict.", Tools: []string{"Read"}, Model: "sonnet", MaxTurns: 3}

// claude renders only the selected persona; the others get it as system text with the lost fields named.
func TestPersonaClaudeNativeAndEmulated(t *testing.T) {
	args := claudeArgs(nil, RunOptions{Persona: testPersona})
	i := slices.Index(args, "--agents")
	if i < 0 || !slices.Contains(args, "--agent") || args[slices.Index(args, "--agent")+1] != "reviewer" {
		t.Fatalf("args = %q", args)
	}
	var defs map[string]map[string]any
	if err := json.Unmarshal([]byte(args[i+1]), &defs); err != nil || len(defs) != 1 {
		t.Fatalf("--agents = %q (%v), want exactly the selected persona", args[i+1], err)
	}
	if d := defs["reviewer"]; d["prompt"] != "Be strict." || d["model"] != "sonnet" || d["maxTurns"] != float64(3) {
		t.Errorf("definition = %v", d)
	}

	req := RunOptions{Prompt: "T", Persona: testPersona}
	out, dropped, emulated := applyCaps(Caps{Persona: Emulated, AppendSystemPrompt: Emulated}, req)
	if out.Persona != nil || !strings.Contains(out.Prompt, "Be strict.") || !strings.HasSuffix(out.Prompt, "T") {
		t.Errorf("emulated prompt = %q persona=%v", out.Prompt, out.Persona)
	}
	if !slices.Contains(emulated, "persona") || len(dropped) != 3 {
		t.Errorf("emulated=%q dropped=%q, want --persona and tools/model/max_turns dropped", emulated, dropped)
	}
	out, _, _ = applyCaps(Caps{Persona: Emulated, AppendSystemPrompt: Native}, req)
	if out.Persona != nil || !strings.Contains(out.AppendSystemPrompt, "Be strict.") {
		t.Errorf("native-append route = %+v", out)
	}
	if _, dropped, _ := applyCaps(Caps{}, req); !slices.Equal(dropped, []string{"persona=reviewer"}) {
		t.Errorf("no channel: dropped = %q", dropped)
	}
}

func TestLoadSchemaUnknown(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, ref := range []string{"missing", "../x", "/no/such.md", "bad name"} {
		if p, err := LoadSchema(ref); err == nil {
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
	p, err := LoadSchema(f)
	if err != nil || p.Tools == nil || len(p.Tools) != 0 {
		t.Fatalf("loaded %+v (%v), want non-nil empty Tools", p, err)
	}
	if args := personaArgs(p); !strings.Contains(args[1], `"tools":[]`) {
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
		"sec":   {Name: "sec", Prompt: "S.", Runner: "codex"},
		"plain": {Name: "plain", Prompt: "P."},
		"help":  {Name: "help", Prompt: "H.", Runner: "gemini"},
	}
	for _, c := range []struct{ ref, runner, persona string }{
		{"codex", "codex", ""},              // the roster name beats agents/codex.md
		{"sec", "codex", "sec"},             // an agents key resolves to def.Runner
		{"plain", "claude", "plain"},        // an agents key with no runner runs on claude
		{"codex:helper", "codex", "helper"}, // vendor:name loads agents/<name>.md
		{"codex:help", "codex", "help"},     // the vendor beats def.Runner
		{"helper", "claude", "helper"},      // agents/<name>.md with no runner runs on claude
		{file, "claude", "x"},               // a file path
	} {
		v, p, err := SelectAgent(c.ref, agents)
		if err != nil || v != c.runner || personaName(p) != c.persona {
			t.Errorf("SelectAgent(%q) = %q, %+v, %v; want %q persona %q", c.ref, v, p, err, c.runner, c.persona)
		}
	}
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
		if err := RefusePersona(c.caps, "codex", p(c.tools)); (err != nil) != c.refuse {
			t.Errorf("%s: err = %v, refuse want %v", c.name, err, c.refuse)
		}
	}
	if err := RefusePersona(emu, "codex", nil); err != nil {
		t.Errorf("nil persona: %v", err)
	}
}

// A hop to an emulating agent must not run a tool-limited persona unrestricted.
func TestFallbackHopRefusesRestrictivePersona(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	down := &fakeRunner{name: "codex", errs: []error{errors.New(`exec: "codex": executable file not found in $PATH`)}, emptyAnswer: true}
	emu := &fakeRunner{name: "gemini", caps: Caps{Persona: Emulated, AppendSystemPrompt: Emulated}, errs: []error{nil}}
	res, err := RunWithFallback(context.Background(), chainOf([]Runner{down, emu}), RunOptions{Prompt: "p", Persona: testPersona}, Discard, nil)
	if err == nil || len(emu.seen) != 0 || len(res.Hops) != 2 {
		t.Fatalf("agent=%q err=%v emulating runs=%d, want a refusal on the second hop and no run", res.Agent, err, len(emu.seen))
	}
	if !strings.Contains(err.Error(), "cannot enforce") {
		t.Errorf("err = %v \n%d hops, last %s/%s, want the persona refusal", err, len(res.Hops), res.Hops[len(res.Hops)-1].Agent, res.Hops[len(res.Hops)-1].ErrorKind)
	}
}
