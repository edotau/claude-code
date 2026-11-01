package agent

import (
	"context"
	"slices"
	"testing"

	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// Interactive codex (nil Headless) carries the CODEX_EFFORT pin; headless Effort still wins.
func TestCodexInteractiveCarriesEffortPin(t *testing.T) {
	tg := target(t, "openai", "gpt-5")
	vaulttest.SetRecord(t, "codex", "effort", "medium")
	_, argv, err := schemaFor("codex").Configure(context.Background(), tg, "")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(argv, `model_reasoning_effort="medium"`) {
		t.Errorf("interactive argv = %q, want the effort pin", argv)
	}
	_, argv, err = schemaFor("codex").configureHeadless(context.Background(), tg, Headless{Prompt: "hi", Effort: "low"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(argv, `model_reasoning_effort="low"`) || slices.Contains(argv, `model_reasoning_effort="medium"`) {
		t.Errorf("headless argv = %q, want explicit low over the pin", argv)
	}
}
