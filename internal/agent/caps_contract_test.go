package agent

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// The capability matrix end to end: each runner's real Caps through applyCaps, per level.
func TestCapabilityMatrixApplyCaps(t *testing.T) {
	const schema = `{"type": "object"}`
	want := map[string]struct {
		sys, app, schema Level
	}{
		"claude":   {Native, Native, Native},
		"codex":    {Native, Native, Native},
		"gemini":   {Emulated, Emulated, Native},
		"opencode": {Native, Emulated, Emulated},
		"copilot":  {Emulated, Emulated, Emulated},
	}
	for _, r := range Runners() {
		w, ok := want[r.Name()]
		if !ok {
			t.Fatalf("unexpected runner %q", r.Name())
		}
		t.Run(r.Name(), func(t *testing.T) {
			c := r.Caps()
			if c.SystemPrompt != w.sys || c.AppendSystemPrompt != w.app || c.OutputSchema != w.schema || c.Env != Native {
				t.Fatalf("caps sys=%v append=%v schema=%v env=%v, want %v/%v/%v/native", c.SystemPrompt, c.AppendSystemPrompt, c.OutputSchema, c.Env, w.sys, w.app, w.schema)
			}
			in := RunOptions{Prompt: "TASK", SystemPrompt: "SYS", AppendSystemPrompt: "APP", OutputSchema: schema, Env: []string{"A=1"}}
			out, dropped, emulated := applyCaps(c, in)
			if len(dropped) != 0 || !slices.Equal(out.Env, in.Env) {
				t.Errorf("dropped = %v, env = %v; env is native everywhere", dropped, out.Env)
			}
			var wantEm []string
			wantPrompt := "TASK"
			if w.sys == Emulated {
				wantEm = append(wantEm, "--system-prompt")
				wantPrompt = preamble("SYS") + wantPrompt
			}
			if w.app == Emulated {
				wantEm = append(wantEm, "--append-system-prompt")
				wantPrompt = preamble("APP") + wantPrompt
			}
			if w.schema != Native {
				wantEm = append(wantEm, "--output-schema")
				wantPrompt += "\n\nAnswer with only a JSON value matching this JSON Schema, no prose or code fence: " + `{"type":"object"}`
			}
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

// The envelope's new fields are omitted, not null or empty, when unset.
func TestEnvelopeNewFieldsOmitEmpty(t *testing.T) {
	keys := func(env Envelope) map[string]json.RawMessage {
		b, err := json.Marshal(env)
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]json.RawMessage{}
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	empty := keys(NewEnvelope(Result{Agent: "claude", Status: StatusOK, Usage: Usage{InputTokens: 1}}, nil, "d"))
	for _, k := range []string{"emulated", "structured", "schema_unchecked"} {
		if _, ok := empty[k]; ok {
			t.Errorf("%s present on an empty result", k)
		}
	}
	if strings.Contains(string(empty["usage"]), "cost_usd") {
		t.Errorf("usage = %s, cost_usd must be absent when zero", empty["usage"])
	}
	full := keys(NewEnvelope(Result{Agent: "claude", Status: StatusOK, Emulated: []string{"--output-schema"},
		Structured: json.RawMessage(`{"a":1}`), SchemaUnchecked: []string{"pattern"}, Usage: Usage{CostUSD: 0.5}}, nil, "d"))
	for k, v := range map[string]string{"emulated": `["--output-schema"]`, "structured": `{"a":1}`, "schema_unchecked": `["pattern"]`} {
		if string(full[k]) != v {
			t.Errorf("%s = %s, want %s", k, full[k], v)
		}
	}
	if !strings.Contains(string(full["usage"]), `"cost_usd":0.5`) {
		t.Errorf("usage = %s, want cost_usd", full["usage"])
	}
}
