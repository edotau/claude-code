package providers

import (
	"context"
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
		"typo:AUTO":       "typo is not a profile",
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

const overlayForm = `--overlay '{"provider":"databricks","profile":"dev"}' --model opus`

// Every Inventory A input here refuses `dev:opus` naming the overlay form and passes a local tag.
func TestNoPrefixedModelAtAnyProviderInput(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	reg := &Registry{Default: "dev", Providers: map[string]*Provider{"dev": {Name: "dev", Provider: "databricks"}}}
	inputs := map[string]func(model string) error{
		"CheckModel": func(m string) error { return CheckModel(reg, m) },
		"ParseModelPins": func(m string) error {
			_, err := ParseModelPins(reg, "", false, []string{"opus=" + m})
			return err
		},
		"ParseModelPins all=": func(m string) error {
			_, err := ParseModelPins(reg, "", false, []string{"all=" + m})
			return err
		},
		"EnvModel": func(m string) error {
			t.Setenv("CODEX_MODEL", m)
			got, _ := EnvModel(context.Background(), "codex")
			return CheckModel(reg, got)
		},
	}
	for name, run := range inputs {
		t.Run(name, func(t *testing.T) {
			if err := run("dev:opus"); err == nil || !strings.Contains(err.Error(), overlayForm) || !errors.Is(err, ErrModelSpec) {
				t.Fatalf("dev:opus = %v; want %q", err, overlayForm)
			}
			if err := run("qwen3:30b"); err != nil {
				t.Fatalf("qwen3:30b = %v; want nil", err)
			}
		})
	}
	t.Run("sessionPin ignores a prefixed pin", func(t *testing.T) {
		if got := SessionPin(reg, "dev:opus"); got != "" {
			t.Fatalf("sessionPin = %q; want empty", got)
		}
		if got := SessionPin(reg, "opus"); got != "opus" {
			t.Fatalf("SessionPin(opus) = %q", got)
		}
	})
}

func TestNoPrefixedModelAtSharedInput(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	want := `--overlay '{"provider":"databricks","profile":"auto"}' --model opus`
	if err := CheckModelShared("auto:opus"); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("auto:opus = %v; want %q", err, want)
	}
	if err := CheckModelShared("qwen3:30b"); err != nil {
		t.Fatalf("qwen3:30b = %v", err)
	}
}
