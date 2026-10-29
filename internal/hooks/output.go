package hooks

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

func decisionName(d Decision, askAsDeny bool) string {
	switch {
	case d == Block, d == Ask && askAsDeny:
		return "deny"
	case d == Ask:
		return "ask"
	}
	return ""
}

// OutputClaude is Claude Code's hookSpecificOutput document; "" when the result says nothing.
func OutputClaude(event string, r Result) string {
	if r.Stdout != "" {
		return r.Stdout
	}
	inner := map[string]any{}
	if event == "PreToolUse" {
		if d := decisionName(r.Decision, false); d != "" {
			inner["permissionDecision"] = d
			inner["permissionDecisionReason"] = r.Reason
		}
		if r.UpdatedInput != nil {
			inner["updatedInput"] = r.UpdatedInput
		}
	}
	if r.Context != "" {
		inner["additionalContext"] = r.Context
	}
	doc := map[string]any{}
	if len(inner) > 0 {
		inner["hookEventName"] = event
		doc["hookSpecificOutput"] = inner
	}
	if r.SystemMessage != "" {
		doc["systemMessage"] = r.SystemMessage
	}
	return marshalDoc(doc)
}

// OutputCodex is exactly one JSON document in Codex's Claude-compatible schema.
func OutputCodex(event string, r Result) string { return OutputClaude(event, r) }

// OutputCopilot is Copilot's flat shape; it has no ask, so ask is deny.
func OutputCopilot(event string, r Result) string {
	doc := map[string]any{}
	if event == "PreToolUse" {
		if d := decisionName(r.Decision, true); d != "" {
			doc["permissionDecision"] = d
			doc["permissionDecisionReason"] = r.Reason
		}
	}
	if r.Context != "" {
		doc["additionalContext"] = r.Context
	}
	return marshalDoc(doc)
}

func marshalDoc(doc map[string]any) string {
	if len(doc) == 0 {
		return ""
	}
	b, _ := json.Marshal(doc)
	return string(b)
}

// Emit writes a merged Result for a vendor and returns the exit code: a block outside PreToolUse, and a Copilot or
// Claude block with no JSON channel, is exit 2 with the reason on stderr.
func Emit(vendor, event string, r Result, stdout, stderr io.Writer) int {
	build := OutputClaude
	switch vendor {
	case VendorCodex:
		build = OutputCodex
	case VendorCopilot:
		build = OutputCopilot
	}
	if r.Stderr != "" {
		fmt.Fprintln(stderr, r.Stderr)
	}
	if r.ExitCode != 0 {
		return r.ExitCode
	}
	if r.Decision == Block && event != "PreToolUse" {
		fmt.Fprintln(stderr, strings.TrimSpace(r.Reason))
		return ExitBlock
	}
	if out := build(event, r); out != "" {
		fmt.Fprintln(stdout, out)
	}
	return ExitProceed
}
