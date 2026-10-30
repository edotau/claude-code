package models

import "testing"

// spellings is the schema plan's "Spellings of one model" table: rule -> wire id, per canonical model.
var spellings = []struct {
	canonical string
	wire      map[string]string
}{
	{"claude-opus-5-5", map[string]string{
		"anthropic": "claude-opus-5-5", "databricks-serving": "databricks-claude-opus-5-5",
		"databricks-ai-gateway": "system.ai.claude-opus-5-5", "dotted": "claude-opus-5.5"}},
	{"gpt-6-1-sol", map[string]string{
		"databricks-serving": "databricks-gpt-6-1-sol", "databricks-ai-gateway": "system.ai.gpt-6-1-sol",
		"dotted": "gpt-6.1-sol"}},
	{"gemini-3-8-flash", map[string]string{
		"databricks-serving": "databricks-gemini-3-8-flash", "dotted": "gemini-3.8-flash"}},
	{"kimi-k3-1-pro", map[string]string{"dotted": "kimi-k3.1-pro"}},
	{"glm-5-3", map[string]string{"anthropic": "glm-5-3", "dotted": "glm-5.3"}},
	{"qwen35-122b-a10b", map[string]string{"dotted": "qwen35-122b-a10b"}},
	{"deepseek-v4-pro-0813", map[string]string{"dotted": "deepseek-v4-pro-0813"}},
}

func TestWireAndResolveRoundTrip(t *testing.T) {
	for _, m := range spellings {
		for rule, w := range m.wire {
			if got := Wire(rule, m.canonical); got != w {
				t.Errorf("Wire(%s, %s) = %q, want %q", rule, m.canonical, got, w)
			}
			for _, in := range []string{w, w + "[1m]"} {
				got, oneM, ok := Resolve(in)
				if !ok || got != m.canonical || oneM != (in != w) {
					t.Errorf("Resolve(%q) = %q,%v,%v; want %q", in, got, oneM, ok, m.canonical)
				}
			}
		}
	}
}

func TestResolveForms(t *testing.T) {
	cases := map[string]string{
		"opus": "claude-opus-5-5", "Sonnet": "claude-sonnet-5-5", "haiku": "claude-haiku-5-5",
		"fable": "claude-fable-5-1", "opus-5-5": "claude-opus-5-5", "opus-5.5": "claude-opus-5-5",
		"claude-sonnet-4.6":              "claude-sonnet-4-6", // B4: not 4.0
		"databricks-claude-opus-4-8":     "claude-opus-4-8",
		"anthropic/claude-opus-4-8":      "claude-opus-4-8",
		"claude-haiku-4-5-20251001":      "claude-haiku-4-5-20251001",
		"claude-sonnet-4-20250514":       "claude-sonnet-4-20250514",
		"opencode/gpt-6.1-sol":           "gpt-6-1-sol",
		"github-copilot/claude-opus-5.5": "claude-opus-5-5",
		"nvidia/z-ai/glm-5.3":            "glm-5-3",
		"opencode-go/kimi-k3.1-pro":      "kimi-k3-1-pro",
	}
	for in, want := range cases {
		if got, _, ok := Resolve(in); !ok || got != want {
			t.Errorf("Resolve(%q) = %q,%v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "my-endpoint", "databricks-my-endpoint", "gpt-9-9"} {
		if got, _, ok := Resolve(in); ok {
			t.Errorf("Resolve(%q) = %q, want not ok", in, got)
		}
	}
}

func TestDottedOneM(t *testing.T) {
	if _, maj, min, _ := ParseClaudeID("claude-opus-5.5"); maj != 5 || min != 5 {
		t.Errorf("claude-opus-5.5 parsed %d.%d", maj, min)
	}
}

func TestAliasAndLookup(t *testing.T) {
	for _, a := range Slots {
		id, ok := Alias(a)
		_, found := Lookup(id)
		if !ok || !found || AliasOf(id) != a || FamilyOf(id) != a {
			t.Errorf("alias %s -> %q ok=%v found=%v", a, id, ok, found)
		}
	}
	if e, _ := Lookup("claude-opus-5-5"); e.Name == "" {
		t.Errorf("opus entry = %+v", e)
	}
	if e, _ := Lookup("gpt-6-1-sol"); !e.ResponsesOnly {
		t.Errorf("gpt-6-1-sol entry = %+v", e)
	}
	if got := Wire("no-such-rule", "gpt-6-1-sol"); got != "gpt-6-1-sol" {
		t.Errorf("unknown rule = %q", got)
	}
}

func TestCatalogIntegrity(t *testing.T) {
	seen := map[string]string{}
	for _, e := range load().Models {
		for rule := range load().Spellings {
			w := Wire(rule, e.ID)
			if prev, dup := seen[w]; dup && prev != e.ID {
				t.Errorf("wire %q is both %s and %s", w, prev, e.ID)
			}
			seen[w] = e.ID
		}
	}
}
