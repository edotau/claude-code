// Package hookspec declares the harness hook chain and renders settings.json's "hooks" block.
package hookspec

import (
	"bytes"
	"encoding/json"
	"strings"
)

// HookBin is the literal binary reference every harness command starts with; $HOME expands under `sh -c`.
const HookBin = "$HOME/.claude/bin/claude-code"

// Claude Code lifecycle event names.
const (
	EventPreToolUse   = "PreToolUse"
	EventSessionStart = "SessionStart"
	EventStop         = "Stop"
	EventSessionEnd   = "SessionEnd"
)

// Spec declares one `claude-code hook <Name>` invocation.
type Spec struct {
	Name    string
	Event   string
	Matcher string // "" = no matcher
	Timeout int    // seconds; always rendered
}

// Command is the shell command settings.json carries.
func (s Spec) Command() string { return HookBin + " hook " + s.Name }

// IsHarness reports whether a settings.json hook command belongs to this harness.
func IsHarness(command string) bool {
	return strings.HasPrefix(strings.TrimSpace(command), HookBin+" ")
}

// Registry is the whole chain in per-event execution order.
var Registry = []Spec{
	{Name: "safety", Event: EventPreToolUse, Matcher: "Bash", Timeout: 5},
	{Name: "context-checkpoint", Event: EventStop, Timeout: 10},
	{Name: "stop-format", Event: EventStop, Timeout: 30},
	{Name: "session-harvest", Event: EventStop, Timeout: 15},
	{Name: "session-start", Event: EventSessionStart, Timeout: 10},
	{Name: "session-harvest-end", Event: EventSessionEnd, Timeout: 15},
}

// Hook and Group are the settings.json hook schema.
type Hook struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}

type Group struct {
	Matcher string `json:"matcher,omitempty"`
	Hooks   []Hook `json:"hooks"`
}

// Events renders the registry as event → groups, collapsing consecutive specs sharing a matcher.
func Events() map[string][]Group {
	block := map[string][]Group{}
	for _, s := range Registry {
		groups := block[s.Event]
		h := Hook{Type: "command", Command: s.Command(), Timeout: s.Timeout}
		if n := len(groups); n > 0 && groups[n-1].Matcher == s.Matcher {
			groups[n-1].Hooks = append(groups[n-1].Hooks, h)
			continue
		}
		block[s.Event] = append(groups, Group{Matcher: s.Matcher, Hooks: []Hook{h}})
	}
	return block
}

// SettingsJSON renders the object under settings.json's "hooks" key.
func SettingsJSON() ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	err := enc.Encode(Events())
	return bytes.TrimSpace(buf.Bytes()), err
}
