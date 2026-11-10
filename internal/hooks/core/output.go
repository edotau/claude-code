package core

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
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

// claudeStyle is how an agent's Claude-compatible document deviates from Claude's own.
type claudeStyle struct {
	askAsDeny      bool // agent has no ask decision
	allowWithInput bool // an updatedInput on PreToolUse is an explicit allow
}

// claudeDoc renders the hookSpecificOutput document with one agent's deviations from Claude's shape.
func claudeDoc(event string, r Result, style claudeStyle) string {
	if r.Stdout != "" {
		return r.Stdout
	}
	inner := map[string]any{}
	if event == "PreToolUse" {
		switch d := decisionName(r.Decision, style.askAsDeny); {
		case d != "":
			inner["permissionDecision"] = d
			inner["permissionDecisionReason"] = r.Reason
		case style.allowWithInput && r.UpdatedInput != nil:
			inner["permissionDecision"] = "allow"
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

// OutputGemini is agy's PreToolUse document: a deny carries the reason; anything else is silent so agy's own gate decides.
func OutputGemini(event string, r Result) string {
	if event != "PreToolUse" || r.Decision != Block {
		return ""
	}
	return marshalDoc(map[string]any{"decision": "deny", "reason": r.Reason})
}

func marshalDoc(doc map[string]any) string {
	if len(doc) == 0 {
		return ""
	}
	b, _ := json.Marshal(doc)
	return string(b)
}

// Emit writes a merged Result and returns the exit code; a block with no JSON channel, or a failed stdout write, is exit 2.
func Emit(name, event string, r Result, stdout, stderr io.Writer) int {
	build := func(event string, r Result) string { return claudeDoc(event, r, claudeStyle{}) }
	switch name {
	case AgentCodex:
		build = OutputCodex
	case AgentCopilot:
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
		if _, err := fmt.Fprintln(stdout, out); err != nil {
			fmt.Fprintf(stderr, "hook: write output: %v\n", err)
			return ExitBlock
		}
	}
	return ExitProceed
}

// OutputCodex is exactly one JSON document in Codex's Claude-compatible schema: no ask decision (deny), and an
// updatedInput on PreToolUse is an explicit allow (ruling 4).
func OutputCodex(event string, r Result) string {
	return claudeDoc(event, r, claudeStyle{askAsDeny: true, allowWithInput: true})
}

// Decision is a hook's verdict on a tool call; the strictest across groups wins.
type Decision int

const (
	Allow Decision = iota
	Ask
	Block
)

// Result is what one handler contributes to its event; the dispatcher merges them and an agent builder renders it.
type Result struct {
	Decision      Decision
	Reason        string
	Context       string // model-visible additionalContext
	SystemMessage string // user-visible note
	UpdatedInput  map[string]any
	Stdout        string // raw stdout: only the worktree group may set it
	Stderr        string
	ExitCode      int // non-zero exit other than 2; honored only from the worktree group
}

// Runner is one handler bound to an event.
type Runner func(*Input) Result

// ExitResult maps a helper's exit code and stderr text to a Result: 2 blocks with the text, other codes pass through.
func ExitResult(code int, stderr string) Result {
	stderr = strings.TrimSpace(stderr)
	switch code {
	case ExitProceed:
		return Result{Stderr: stderr}
	case ExitBlock:
		return Result{Decision: Block, Reason: stderr}
	}
	return Result{Stderr: stderr, ExitCode: code}
}

// WithStderr returns r with err's text appended to its stderr; a nil err returns r unchanged.
func WithStderr(r Result, err error) Result {
	if err != nil {
		r.Stderr = strings.TrimSpace(r.Stderr + "\n" + err.Error())
	}
	return r
}

// JoinContext joins the non-empty context blocks into one additionalContext text.
func JoinContext(blocks ...string) string {
	return strings.Join(slices.DeleteFunc(slices.Clone(blocks), func(b string) bool { return b == "" }), "\n\n")
}
