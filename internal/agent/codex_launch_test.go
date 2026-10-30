package agent

import (
	"context"
	"github.com/edotau/claude-code/internal/providers"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestCodexChatGPTLogin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // no bin/codex: the auth.json fallback is what this test exercises
	t.Setenv("PATH", t.TempDir())
	tg := target(t, "chatgpt", "gpt-5.5")
	if _, _, err := Configure(context.Background(), "codex", tg, ""); err == nil || !strings.Contains(err.Error(), "codex login") {
		t.Fatalf("no auth.json: err = %v, want a codex login hint", err)
	}
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"OPENAI_API_KEY":null,"tokens":{"id_token":"x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	env, argv, err := ConfigureHeadless(context.Background(), "codex", tg, Headless{Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"exec", "-c", `model_provider="openai"`, "-c", `model="gpt-5.5"`, "--skip-git-repo-check", "--sandbox", "read-only", "-"}
	if len(env) != 0 || !slices.Equal(argv, want) {
		t.Errorf("env %q argv %q, want no env and %q", env, argv, want)
	}
	if _, _, err := Configure(context.Background(), "gemini", tg, ""); err == nil || !strings.Contains(err.Error(), "codex sign-in") {
		t.Errorf("only codex may use the chatgpt login: %v", err)
	}
}

// codexHeadless carries CODEX_EFFORT into -c model_reasoning_effort when the call names no
// explicit Effort; an explicit Headless.Effort still wins over the pin (TestConfigure's codex/Effort
// case already proves the no-pin explicit path, so this proves the pin-only and pin-vs-explicit paths).
func TestCodexHeadlessEffortPinUsedWhenEffortEmptyExplicitWinsOverIt(t *testing.T) {
	tg := target(t, "openai", "gpt-5")
	setPin(t, providers.PinCodexEffort, "")
	if err := providers.SetPins(map[string]string{providers.PinCodexEffort: ""}); err != nil {
		t.Fatal(err)
	}

	_, argv, err := ConfigureHeadless(context.Background(), "codex", tg, Headless{Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(argv, "model_reasoning_effort=\"\"") || slices.ContainsFunc(argv, func(s string) bool { return strings.Contains(s, "model_reasoning_effort=") }) {
		t.Errorf("argv %q with no pin and no explicit effort, want no model_reasoning_effort flag", argv)
	}

	if err := providers.SetPins(map[string]string{providers.PinCodexEffort: "xhigh"}); err != nil {
		t.Fatal(err)
	}
	_, argv, err = ConfigureHeadless(context.Background(), "codex", tg, Headless{Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(argv, `model_reasoning_effort="xhigh"`) {
		t.Errorf("argv = %q, want the CODEX_EFFORT pin (xhigh) carried in", argv)
	}

	// An explicit Headless.Effort still wins over the pin.
	_, argv, err = ConfigureHeadless(context.Background(), "codex", tg, Headless{Prompt: "hi", Effort: "low"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(argv, `model_reasoning_effort="low"`) || slices.Contains(argv, `model_reasoning_effort="xhigh"`) {
		t.Errorf("argv = %q, want the explicit effort (low) to beat the xhigh pin", argv)
	}
}
