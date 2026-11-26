// Package secrets masks vault values and credential shapes in tool output and ended transcripts.
package secrets

import (
	"fmt"
	"maps"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/redact"
)

// VaultWait gives up on the vault read with a quarter of the budget left, so a stalled keyring never skips masking.
var VaultWait = func() time.Duration { s, _ := core.SpecOf("secrets-out"); return core.BudgetOf(s) * 3 / 4 }

const shapesOnlyMsg = "claude-code: secrets-out: vault values could not be loaded (%s); only credential-shaped text was masked"

const shapesOnlyNote = "claude-code: secrets-out: vault values unavailable; masked credential shapes only"

// Out (PostToolUse Bash) masks vault values and credential shapes at the same length; other calls return nothing.
func Out(in *core.Input) core.Result {
	resp, ok := in.Field("tool_response").(map[string]any)
	if !ok {
		return core.Result{}
	}
	texts := map[string]string{}
	for _, key := range []string{"stdout", "stderr"} {
		if text, ok := resp[key].(string); ok && len(text) >= redact.MinLen { // no vault value is shorter
			texts[key] = text
		}
	}
	if len(texts) == 0 {
		return core.Result{}
	}
	mask, note, msg := outputMask()
	var updated map[string]any
	for key, text := range texts {
		if b := []byte(text); mask(b) > 0 {
			if updated == nil {
				updated = maps.Clone(resp)
			}
			updated[key] = string(b) // only a string that held a value changes; other keys, types and absence stay
		}
	}
	if updated == nil {
		return core.Result{Stderr: note, SystemMessage: msg}
	}
	return core.Result{UpdatedToolOutput: updated, Stderr: note, SystemMessage: msg}
}

// outputMask masks values and shapes; vault unreadable or slow: shapes alone, a note, a message once a minute.
func outputMask() (mask func([]byte) int, note, msg string) {
	m, cause, announce := redact.SharedWithin(VaultWait())
	if cause != "" {
		note = shapesOnlyNote
		if announce {
			msg = fmt.Sprintf(shapesOnlyMsg, cause)
		}
	}
	return m.Mask, note, msg
}
