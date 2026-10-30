package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var testPersona = &Persona{Name: "reviewer", Description: "Reviews", Prompt: "Be strict.", Tools: []string{"Read"}, Model: "sonnet", MaxTurns: 3}

// claude renders only the selected persona; the others get it as system text with the lost fields named.
func TestPersonaClaudeNativeAndEmulated(t *testing.T) {
	args := claudeArgs(nil, Request{Persona: testPersona})
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

	req := Request{Prompt: "T", Persona: testPersona}
	out, dropped, emulated := applyCaps(Caps{Persona: Emulated, AppendSystemPrompt: Emulated}, req)
	if out.Persona != nil || !strings.Contains(out.Prompt, "Be strict.") || !strings.HasSuffix(out.Prompt, "T") {
		t.Errorf("emulated prompt = %q persona=%v", out.Prompt, out.Persona)
	}
	if !slices.Contains(emulated, "--persona") || len(dropped) != 3 {
		t.Errorf("emulated=%q dropped=%q, want --persona and tools/model/max_turns dropped", emulated, dropped)
	}
	out, _, _ = applyCaps(Caps{Persona: Emulated, AppendSystemPrompt: Native}, req)
	if out.Persona != nil || !strings.Contains(out.AppendSystemPrompt, "Be strict.") {
		t.Errorf("native-append route = %+v", out)
	}
	if _, dropped, _ := applyCaps(Caps{}, req); !slices.Equal(dropped, []string{"--persona=reviewer"}) {
		t.Errorf("no channel: dropped = %q", dropped)
	}
}

func TestLoadPersonaUnknown(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, ref := range []string{"missing", "../x", "/no/such.md", "bad name"} {
		if p, err := LoadPersona(ref); err == nil {
			t.Errorf("LoadPersona(%q) = %+v, want an error", ref, p)
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
	p, err := LoadPersona(f)
	if err != nil || p.Tools == nil || len(p.Tools) != 0 {
		t.Fatalf("loaded %+v (%v), want non-nil empty Tools", p, err)
	}
	if args := personaArgs(*p); !strings.Contains(args[1], `"tools":[]`) {
		t.Errorf("--agents = %s, want tools:[]", args[1])
	}
	b, _ := json.Marshal(Request{Persona: p})
	var back Request
	if err := json.Unmarshal(b, &back); err != nil || back.Persona.Tools == nil {
		t.Errorf("round-trip lost the empty list: %s (%v)", b, err)
	}
	if _, dropped, _ := applyCaps(Caps{Persona: Emulated, AppendSystemPrompt: Emulated}, Request{Persona: p}); !slices.Contains(dropped, "--persona.tools=none") {
		t.Errorf("dropped = %q, want tools=none named", dropped)
	}
}
