package providers

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const overlayForm = `--overlay '{"provider":"databricks","profile":"dev"}' --model opus`

// Every Inventory A input reachable from this package refuses `dev:opus` naming the overlay form and passes a local tag.
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
		if got := sessionPin(reg, "dev:opus"); got != "" {
			t.Fatalf("sessionPin = %q; want empty", got)
		}
		if got := sessionPin(reg, "opus"); got != "opus" {
			t.Fatalf("sessionPin(opus) = %q", got)
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
