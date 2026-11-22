package models

import (
	"strings"
	"testing" // tests refuse real DNS: mock API calls

	_ "github.com/edotau/claude-code/internal/nonet"
)

// spellings is the schema plan's "Spellings of one model" table: rule -> wire id, per canonical model.
var spellings = []struct {
	canonical string
	wire      map[string]string
}{
	{"claude-opus-5-5", map[string]string{
		"anthropic": "claude-opus-5-5", "databricks-serving": "databricks-claude-opus-5-5",
		"dotted": "claude-opus-5.5"}},
	{"gpt-6-1-sol", map[string]string{
		"databricks-serving": "databricks-gpt-6-1-sol", "dotted": "gpt-6.1-sol"}},
	{"gemini-3-8-flash", map[string]string{
		"databricks-serving": "databricks-gemini-3-8-flash", "dotted": "gemini-3.8-flash"}},
	{"claude-sonnet-5-5", map[string]string{"agy": "claude-sonnet-5-5"}},
	{"gpt-oss-120b", map[string]string{"agy": "gpt-oss-120b"}},
	{"gemini-3-8-flash", map[string]string{"agy": "gemini-3.8-flash"}},
	{"claude-haiku-4-5-20251001", map[string]string{"anthropic": "claude-haiku-4-5-20251001"}},
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
				if !ok || got != m.canonical || oneM {
					t.Errorf("Resolve(%q) = %q,%v,%v; want %q", in, got, oneM, ok, m.canonical)
				}
			}
		}
	}
}

func TestAcceptedEfforts(t *testing.T) {
	for id, want := range map[string]string{"gemini-4-hydrogen": "low medium high xhigh", "gemini-3.8-flash": "low medium high",
		"claude-sonnet-5-5": "low medium high", "gpt-oss-120b": "medium", "gpt-6-1-sol": ""} {
		if got := strings.Join(AcceptedEfforts("gemini", id), " "); got != want {
			t.Errorf("AcceptedEfforts(gemini, %s) = %q, want %q", id, got, want)
		}
	}
}

func TestAgentModels(t *testing.T) {
	got := strings.Join(AgentModels("gemini"), " ")
	for _, id := range []string{"gemini-4-argon", "claude-opus-5-5", "gpt-oss-120b"} {
		if !strings.Contains(got, id) {
			t.Errorf("AgentModels(gemini) = %q, missing %s", got, id)
		}
	}
	if strings.Contains(got, "gemini-3-7-flash") || len(AgentModels("nobody")) != 0 {
		t.Errorf("AgentModels listed a model with no gemini efforts: %q", got)
	}
}

func TestResolveForms(t *testing.T) {
	cases := map[string]string{
		"opus": "claude-opus-5-5", "Sonnet": "claude-sonnet-5-5", "haiku": "claude-haiku-5-5",
		"fable": "claude-fable-5-2", "opus-5-5": "claude-opus-5-5", "opus-5.5": "claude-opus-5-5",
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
	for _, a := range AliasNames {
		id, ok := Alias(a)
		_, found := load().byID[id]
		if !ok || !found || AliasOf(id) != a || FamilyOf(id) != a {
			t.Errorf("alias %s -> %q ok=%v found=%v", a, id, ok, found)
		}
	}
	if e := load().byID["gpt-6-1-sol"]; !e.ResponsesOnly {
		t.Errorf("gpt-6-1-sol entry = %+v", e)
	}
	if got := Wire("no-such-rule", "gpt-6-1-sol"); got != "gpt-6-1-sol" {
		t.Errorf("unknown rule = %q", got)
	}
	if got := Wire("databricks-serving", "claude-haiku-4-5-20251001"); got != "databricks-claude-haiku-4-5" {
		t.Errorf("dated id on the serving rule = %q", got)
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

func TestLimitsResolvesSpellings(t *testing.T) {
	for _, c := range []struct {
		id           string
		context, out int
	}{
		{"claude-sonnet-5-5", 1_000_000, 128_000},
		{"databricks-claude-sonnet-5-5", 1_000_000, 128_000},
		{"claude-sonnet-5-5[1m]", 1_000_000, 128_000},
		{"gpt-6-sol", 1_050_000, 128_000},
		{"gpt-6-1-sol", 1_050_000, 128_000},
		{"databricks-gpt-6-1-sol", 1_050_000, 128_000},
	} {
		if l, ok := Limits(c.id); !ok || l.Context != c.context || l.Output != c.out {
			t.Errorf("Limits(%q) = %+v, %v", c.id, l, ok)
		}
	}
	for _, id := range []string{"gpt-5", "gpt-6-2-terra", "not-a-model", ""} {
		if l, ok := Limits(id); ok {
			t.Errorf("Limits(%q) = %+v, want unknown", id, l)
		}
	}
}

// One model compares equal across every spelling; distinct versions and endpoints never do.
func TestSameModel(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"claude-opus-5", "databricks-claude-opus-5", true},
		{"opus", "databricks-claude-opus-5-5[1m]", true},
		{"gpt-6-1-terra", "databricks-gpt-6-1-terra", true},
		{"GPT-5.5", "databricks-gpt-5-5", true},
		{"claude-opus-5", "claude-opus-5-5", false},
		{"databricks-gpt-6-1-terra", "databricks-gpt-6-1-sol", false},
	} {
		if got := SameModel(c.a, c.b); got != c.want {
			t.Errorf("SameModel(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// Each description form yields its numbers; anything else is unknown, never a guess.
func TestParseLimits(t *testing.T) {
	for _, c := range []struct {
		desc         string
		context, out int
	}{
		{"GPT-5.4 features a 400K total token context window with 128K maximum output tokens.", 400000, 128000},
		{"GPT-5 features a 128K token context window.", 128000, 0},
		{"Kimi K3 supports a context length of 1M tokens and more.", 1000000, 0},
		{"Sonnet. It supports a 1 million token context window.", 1000000, 0},
		{"Trained on 10M tokens of data; speeds up to 5K tokens per second.", 0, 0},
		{"A 1.5M token context window.", 0, 0},
		{"", 0, 0},
	} {
		if ctx, out := ParseLimits(c.desc); ctx != c.context || out != c.out {
			t.Errorf("ParseLimits(%q) = %d, %d; want %d, %d", c.desc, ctx, out, c.context, c.out)
		}
	}
}
