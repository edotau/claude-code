package providers

import (
	"errors"
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
