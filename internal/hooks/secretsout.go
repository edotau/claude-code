package hooks

import (
	"maps"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/redact"
)

// SecretsOut (PostToolUse Bash) masks vault values in the output at the same length; any other call returns nothing.
func SecretsOut(in *core.Input) core.Result {
	resp, ok := in.Field("tool_response").(map[string]any)
	if !ok {
		return core.Result{}
	}
	var updated map[string]any
	var m *redact.Matcher
	for _, key := range []string{"stdout", "stderr"} {
		text, ok := resp[key].(string)
		if !ok || len(text) < redact.MinLen { // no vault value is shorter, so short output never opens the vault
			continue
		}
		if m == nil {
			m = redact.Shared()
		}
		if masked, n := maskText(m, text); n > 0 {
			if updated == nil {
				updated = maps.Clone(resp)
			}
			updated[key] = masked // only a string that held a value changes; other keys, types and absence stay
		}
	}
	if updated == nil {
		return core.Result{}
	}
	return core.Result{UpdatedToolOutput: updated}
}

// maskText masks vault values only: shapes would blank ordinary look-alike strings in command output.
func maskText(m *redact.Matcher, s string) (string, int) {
	b := []byte(s)
	n := m.MaskValues(b)
	if n == 0 {
		return s, 0
	}
	return string(b), n
}
