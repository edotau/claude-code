package agent

import (
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

// codexRouteDetail/geminiRouteDetail are doctor's own route rows: names/booleans only (never a
// credential), unset means each vendor's own default decides.
func TestCodexRouteDetailShowsPinSource(t *testing.T) {
	hermetic(t, "") // scratch CLAUDE_CONFIG_DIR: SetPins below must never reach the real vault
	t.Setenv(providers.PinCodexModel, "")
	t.Setenv(providers.PinCodexEffort, "")
	if err := providers.SetPins(map[string]string{providers.PinCodexModel: "", providers.PinCodexEffort: ""}); err != nil {
		t.Fatal(err)
	}
	if got, want := codexRouteDetail(), "model=(default) source=default effort=default"; got != want {
		t.Errorf("codexRouteDetail(unset) = %q, want %q", got, want)
	}

	if err := providers.SetPins(map[string]string{providers.PinCodexModel: "gpt-6.1-sol", providers.PinCodexEffort: "high"}); err != nil {
		t.Fatal(err)
	}
	if got, want := codexRouteDetail(), "model=gpt-6.1-sol source=pin effort=high"; got != want {
		t.Errorf("codexRouteDetail(pinned) = %q, want %q", got, want)
	}
}

func TestGeminiRouteDetailShowsPinSource(t *testing.T) {
	hermetic(t, "") // scratch CLAUDE_CONFIG_DIR: SetPins below must never reach the real vault
	t.Setenv(providers.PinGeminiModel, "")
	if err := providers.SetPins(map[string]string{providers.PinGeminiModel: ""}); err != nil {
		t.Fatal(err)
	}
	if got, want := geminiRouteDetail(), "model=(default) source=default"; got != want {
		t.Errorf("geminiRouteDetail(unset) = %q, want %q", got, want)
	}

	if err := providers.SetPins(map[string]string{providers.PinGeminiModel: "gemini-2.5-pro"}); err != nil {
		t.Fatal(err)
	}
	if got, want := geminiRouteDetail(), "model=gemini-2.5-pro source=pin"; got != want {
		t.Errorf("geminiRouteDetail(pinned) = %q, want %q", got, want)
	}
}
