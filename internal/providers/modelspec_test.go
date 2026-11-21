package providers

import (
	"errors"
	"strings"
	"testing"
)

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

// CheckModel refuses a row, type, pool or unknown prefix naming provider and profile; a local tag passes.
func TestCheckModel(t *testing.T) {
	reg := &Registry{Providers: map[string]*Provider{"dev": {Name: "dev", Provider: "databricks"},
		"qa": {Name: "qa", Provider: "databricks"}, "ultra": {Name: "ultra", Provider: "anthropic"}}}
	for in, want := range map[string]string{
		"dev:auto":        `model dev:auto: a profile is not part of a model; use --overlay '{"provider":"databricks","profile":"dev"}' --model auto`,
		"DEV:opus":        `'{"provider":"databricks","profile":"dev"}' --model opus`,
		"databricks:auto": `'{"provider":"databricks","profile":"auto"}' --model auto`,
		"auto:opus":       `'{"provider":"databricks","profile":"auto"}' --model opus`,
		"anthropic:opus":  `'{"provider":"anthropic","profile":"ultra"}' --model opus`,
		"bogus:opus":      "bogus is not a profile",
	} {
		if err := CheckModel(reg, in); err == nil || !strings.Contains(err.Error(), want) || !errors.Is(err, ErrModelSpec) {
			t.Errorf("CheckModel(%q) = %v; want %q", in, err, want)
		}
	}
	for _, in := range []string{"qwen3:30b", "opus", "nvidia/nemotron-4-340b", "claude-opus-5-5[1m]", ""} {
		if err := CheckModel(reg, in); err != nil {
			t.Errorf("CheckModel(%q) = %v; want nil", in, err)
		}
	}
}
