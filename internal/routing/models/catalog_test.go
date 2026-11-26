package models

import (
	"testing" // tests refuse real DNS: mock API calls

	_ "github.com/edotau/claude-code/internal/platform/nonet"
)

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

// One model compares equal across every spelling; distinct versions and endpoints never do.
func TestSameModel(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"claude-opus-5", "databricks-claude-opus-5", true},
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
