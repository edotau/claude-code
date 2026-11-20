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

// CastModel refuses a profile-prefixed model naming the overlay form, and leaves real ids (/, [1m], local tags) alone.
func TestCastModel(t *testing.T) {
	reg := &Registry{Providers: map[string]*Provider{"dev": {Name: "dev", Provider: "databricks"},
		"qa": {Name: "qa", Provider: "databricks"}}}
	for in, want := range map[string]string{
		"dev:auto":        `use --overlay '{"provider":"databricks","profile":"dev","model":"auto"}'`,
		"databricks:auto": `use --overlay '{"provider":"databricks","profile":"auto","model":"auto"}'`,
		"auto:opus":       `use --overlay '{"provider":"databricks","profile":"auto","model":"opus"}'`,
	} {
		if _, err := CastModel(reg, in); err == nil || !strings.Contains(err.Error(), want) ||
			!strings.Contains(err.Error(), "model "+in+": a model name does not carry a profile") || !errors.Is(err, ErrModelSpec) {
			t.Errorf("CastModel(%q) = %v; want the refusal naming %s", in, err, want)
		}
	}
	for _, id := range []string{"nvidia/nemotron-4-340b", "github-copilot/claude-opus-5.5", "claude-opus-5-5[1m]", "qwen3:30b", "auto"} {
		if got, err := CastModel(reg, id); err != nil || got.Model != id {
			t.Errorf("CastModel(%q) = %+v, %v; want it unchanged", id, got, err)
		}
	}
}
