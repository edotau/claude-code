package hooks

import (
	"fmt"
	"maps"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/redact"
)

// vaultWait gives up on the vault read with a quarter of the budget left, so a stalled keyring never skips masking.
var vaultWait = func() time.Duration { return budgetOf(specOf("secrets-out")) * 3 / 4 }

const shapesOnlyMsg = "claude-code: secrets-out: vault values could not be loaded (%s); only credential-shaped text was masked"

const shapesOnlyNote = "claude-code: secrets-out: vault values unavailable; masked credential shapes only"

// SecretsOut (PostToolUse Bash) masks vault values in the output at the same length; any other call returns nothing.
func SecretsOut(in *core.Input) core.Result {
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
		if masked, n := maskText(mask, text); n > 0 {
			if updated == nil {
				updated = maps.Clone(resp)
			}
			updated[key] = masked // only a string that held a value changes; other keys, types and absence stay
		}
	}
	if updated == nil {
		return core.Result{Stderr: note, SystemMessage: msg}
	}
	return core.Result{UpdatedToolOutput: updated, Stderr: note, SystemMessage: msg}
}

// outputMask is the vault-value masker; unreadable or slow, shapes with a note, and a message once a minute.
func outputMask() (mask func([]byte) int, note, msg string) {
	m, cause, announce := redact.SharedWithin(vaultWait())
	if cause == "" {
		return m.MaskValues, "", ""
	}
	if announce {
		msg = fmt.Sprintf(shapesOnlyMsg, cause)
	}
	return m.Mask, shapesOnlyNote, msg
}

// maskText masks s with mask: vault values only, unless the vault is unreadable and shapes stand in.
func maskText(mask func([]byte) int, s string) (string, int) {
	b := []byte(s)
	n := mask(b)
	if n == 0 {
		return s, 0
	}
	return string(b), n
}
