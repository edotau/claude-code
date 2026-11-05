package agentconfig

import (
	"strings"
	"testing"
)

// Harness-owned agent config names are refused by --env and --keep-env; an ordinary name passes.
func TestFlagOverlayRefusesHarnessOwnedNames(t *testing.T) {
	for _, name := range []string{"OPENCODE_PERMISSION", "OPENCODE_CONFIG", "OPENCODE_CONFIG_CONTENT", "OPENCODE_CONFIG_DIR",
		"OPENCODE_DISABLE_MODELS_FETCH", "CODEX_HOME", "COPILOT_HOME", "COPILOT_ALLOW_ALL", "COPILOT_ASSISTED_APPROVAL",
		"COPILOT_CUSTOM_INSTRUCTIONS_DIRS", "HARNESS_FOO"} {
		if _, err := FlagOverlay([]string{name + "=x"}, nil); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("--env %s: err = %v, want a refusal naming it", name, err)
		}
		if _, err := FlagOverlay(nil, []string{name}); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("--keep-env %s: err = %v, want a refusal naming it", name, err)
		}
	}
	if _, err := FlagOverlay([]string{"FOO=bar"}, []string{"BAR"}); err != nil {
		t.Errorf("FOO=bar: %v", err)
	}
}
