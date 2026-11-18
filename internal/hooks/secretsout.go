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
	out, _ := resp["stdout"].(string)
	errText, _ := resp["stderr"].(string)
	// No vault value is shorter than MinLen, so shorter output never opens the vault.
	if len(out) < redact.MinLen && len(errText) < redact.MinLen {
		return core.Result{}
	}
	m := redact.Shared()
	masked, a := maskText(m, out)
	maskedErr, b := maskText(m, errText)
	if a+b == 0 {
		return core.Result{}
	}
	updated := maps.Clone(resp)
	updated["stdout"], updated["stderr"] = masked, maskedErr
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
