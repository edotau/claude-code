package providers

import (
	"errors"
	"strings"
	"testing"
)

func TestSplitModelSpec(t *testing.T) {
	reg := &Registry{Providers: map[string]*Provider{
		"dev":       {Name: "dev", Provider: "databricks"},
		"qa":        {Name: "qa", Provider: "databricks"},
		"business":  {Name: "business", Provider: "github"},
		"personal":  {Name: "personal", Provider: "github"},
		"anthropic": {Name: "anthropic", Provider: "anthropic"},
		"chatgpt":   {Name: "chatgpt", Provider: "openai"},
	}}
	cases := []struct {
		spec string
		want ModelSpec
		err  bool
	}{
		{"dev:opus", ModelSpec{Row: "dev", Model: "opus"}, false},
		{"anthropic:opus", ModelSpec{Row: "anthropic", Model: "opus"}, false},
		{"openai:opus", ModelSpec{Row: "chatgpt", ByType: true, Model: "opus"}, false},
		{"databricks:opus", ModelSpec{Pool: true, Provider: "databricks", Model: "opus"}, false},
		{"auto:opus", ModelSpec{Pool: true, Provider: "databricks", Model: "opus"}, false},
		{"github:opus", ModelSpec{}, true},
		{"bogus:claude-opus-5-5", ModelSpec{}, true},
		{"qwen3:30b", ModelSpec{Model: "qwen3:30b"}, false},
	}
	for _, c := range cases {
		got, err := SplitModelSpec(reg, c.spec)
		if c.err {
			if !errors.Is(err, ErrModelSpec) {
				t.Errorf("%s: err = %v, want ErrModelSpec", c.spec, err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%s: got %+v, %v; want %+v", c.spec, got, err, c.want)
		}
	}
}

func TestWireModelDottedForOpenAIAndGoogle(t *testing.T) {
	for _, c := range []struct{ provider, in, want string }{
		{"openai", "gpt-6-1-sol", "gpt-6.1-sol"},
		{"google", "gemini-3-8-flash", "gemini-3.8-flash"},
	} {
		if got := (Provider{Provider: c.provider, Auth: Auth{Login: "vendor"}}).WireModel(c.in); got != c.want {
			t.Errorf("%s WireModel(%s) = %s, want %s", c.provider, c.in, got, c.want)
		}
	}
}

// CastModel casts a registered prefix (any case) to its route unit, refuses an unknown or chained one, passes local tags.
func TestCastModel(t *testing.T) {
	reg := &Registry{Providers: map[string]*Provider{"dev": {Name: "dev", Provider: "databricks"},
		"qa": {Name: "qa", Provider: "databricks"}, "ultra": {Name: "ultra", Provider: "anthropic"}}}
	for in, want := range map[string]ModelCast{
		"dev:auto":                       {Model: "auto", Provider: "databricks", Profile: "dev"},
		"DEV:opus":                       {Model: "opus", Provider: "databricks", Profile: "dev"},
		"databricks:auto":                {Model: "auto", Provider: "databricks", Profile: "auto"},
		"auto:opus":                      {Model: "opus", Provider: "databricks", Profile: "auto"},
		"anthropic:opus":                 {Model: "opus", Provider: "anthropic", Profile: "ultra"},
		"nvidia/nemotron-4-340b":         {Model: "nvidia/nemotron-4-340b"},
		"github-copilot/claude-opus-5.5": {Model: "github-copilot/claude-opus-5.5"},
		"claude-opus-5-5[1m]":            {Model: "claude-opus-5-5[1m]"},
		"qwen3:30b":                      {Model: "qwen3:30b"},
	} {
		if got, err := CastModel(reg, in); err != nil || got != want {
			t.Errorf("CastModel(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for in, want := range map[string]string{"bogus:auto": "bogus is not a profile", "bogus:opus": "bogus is not a profile",
		"dev:qa:opus": "one profile prefix at most"} {
		if _, err := CastModel(reg, in); err == nil || !strings.Contains(err.Error(), want) || !errors.Is(err, ErrModelSpec) {
			t.Errorf("CastModel(%q) = %v; want %q", in, err, want)
		}
	}
	c := ModelCast{Model: "opus", Provider: "databricks", Profile: "qa"}
	if _, _, err := c.Merge("", "dev"); err == nil || !strings.Contains(err.Error(), "profile qa is outside dev") {
		t.Errorf("Merge(dev) = %v; want the conflict naming both", err)
	}
}
