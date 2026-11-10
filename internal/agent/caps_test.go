package agent

import (
	"reflect"
	"slices"
	"testing"
)

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

// gemini read-only runs with tools on and the read-only deny rules, never refused or dropped to tools-off.
func TestCheckRunGeminiReadOnlyDenies(t *testing.T) {
	g, err := Lookup("gemini")
	if err != nil {
		t.Fatal(err)
	}
	got, err := checkRun(g, RunOptions{Tools: true, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range ReadOnlyDeny {
		if !slices.Contains(got.Permissions.Deny, r) {
			t.Errorf("deny %v lacks %s", got.Permissions.Deny, r)
		}
	}
	if out, dropped, _ := applyCaps(g.Caps(), got); !out.Tools || !out.ReadOnly || len(dropped) != 0 {
		t.Errorf("applyCaps: tools=%v read-only=%v dropped=%v, want both kept", out.Tools, out.ReadOnly, dropped)
	}
}
