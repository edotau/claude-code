// Package hookspec declares the harness hook chain and renders settings.json's "hooks" block.
package hookspec

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
)

// HookBin is the literal binary reference in harness commands; $HOME expands under `sh -c`.
const HookBin = "$HOME/.claude/bin/claude-code"

// VendorEnv identifies the vendor invoking a hook with a shared payload contract.
const VendorEnv = "HARNESS_HOOK_VENDOR"

// Claude Code lifecycle event names.
const (
	EventPreToolUse         = "PreToolUse"
	EventPostToolUse        = "PostToolUse"
	EventPostToolUseFailure = "PostToolUseFailure"
	EventSubagentStop       = "SubagentStop"
	EventSessionStart       = "SessionStart"
	EventUserPromptSubmit   = "UserPromptSubmit"
	EventStop               = "Stop"
	EventSessionEnd         = "SessionEnd"
	EventPreCompact         = "PreCompact"
	EventWorktreeCreate     = "WorktreeCreate"
	EventWorktreeRemove     = "WorktreeRemove"
)

// Spec declares one `claude-code hook <Name>` invocation.
type Spec struct {
	Name          string
	Event         string
	Matcher       string   // "" = no matcher
	Timeout       int      // seconds; always rendered
	Gate          bool     // preserve blocking exits; other Copilot hooks are advisory
	Vendors       []string // explicit allowlist for non-Claude sessions
	Async         bool     // runs in the background: never delays the tool call (only for hooks that cannot block)
	StatusMessage string   // spinner text Claude Code shows while the hook runs
}

// Command is the shell command settings.json carries.
func (s Spec) Command() string { return HookBin + " hook " + s.Name }

var harnessCommand = regexp.MustCompile(`^(?:` + VendorEnv + `=\w+ )?` + regexp.QuoteMeta(HookBin) + ` `)

// IsHarness reports whether a hook command belongs to this harness, including vendor-marked commands.
func IsHarness(command string) bool {
	return harnessCommand.MatchString(strings.TrimSpace(command))
}

// Registry is the whole chain in per-event execution order.
var Registry = []Spec{
	{Name: "bash-guard", Event: EventPreToolUse, Matcher: "Bash", Timeout: 5, StatusMessage: "Checking command safety", Gate: true, Vendors: []string{"codex", "copilot"}},
	// Check markdown placement, then require template headings for a native plan draft's first write.
	{Name: "docs-placement", Event: EventPreToolUse, Matcher: "Write|Edit", Timeout: 5, StatusMessage: "Checking doc placement", Gate: true, Vendors: []string{"codex", "copilot"}},
	// An unset dispatch model is filled from the local prompt-complexity classifier (low→haiku, else sonnet).
	{Name: "agent-effort", Event: EventPreToolUse, Matcher: "Agent|Task", Timeout: 5},
	// An inline Workflow script's unpinned agent() leaves are pinned to sonnet (else they inherit the session model).
	{Name: "workflow-lint", Event: EventPreToolUse, Matcher: "Workflow", Timeout: 5},
	// Machine-wide subagent cap: pre counts and blocks, tag binds the marker to the agent id, post retires it.
	{Name: "agent-inflight", Event: EventPreToolUse, Matcher: "Agent|Task", Timeout: 5},
	{Name: "agent-inflight-tag", Event: EventPostToolUse, Matcher: "Agent|Task", Timeout: 5},
	{Name: "agent-inflight-post", Event: EventPostToolUseFailure, Matcher: "Agent|Task", Timeout: 5},
	// One SubagentStop hook, run in order: an editing subagent's first stop is sent back for one simplify
	// pass, and its inflight slot retires only on the stop that actually ends it.
	{Name: "subagent-stop", Event: EventSubagentStop, Timeout: 15},
	// Every write/edit to a native plan-mode draft is mirrored to its dated, repo-tagged file in plans/.
	{Name: "plan-mirror", Event: EventPostToolUse, Matcher: "Write|Edit", Timeout: 10},
	// An approved plan is filed under plans/, tagged with its repo.
	{Name: "save-plan", Event: EventPostToolUse, Matcher: "ExitPlanMode", Timeout: 10},
	// A model read of a bank file is traced to that bank's access log (memory doctor's hot/cold signal).
	{Name: "access-trace", Event: EventPostToolUse, Matcher: "Read", Timeout: 5, Async: true, Vendors: []string{"codex", "copilot"}},
	{Name: "context-checkpoint", Event: EventStop, Timeout: 10},
	{Name: "stop-format", Event: EventStop, Timeout: 30},
	{Name: "session-harvest", Event: EventStop, Timeout: 15},
	{Name: "session-start", Event: EventSessionStart, Timeout: 10, StatusMessage: "Loading session memory"},
	// A pasted or dragged Windows path (C:\…) resolves to its /mnt form for the model; a no-op off WSL.
	{Name: "wsl-path-normalize", Event: EventUserPromptSubmit, Timeout: 5, Vendors: []string{"codex", "copilot"}},
	// Per-prompt bank recall: advisory and capped, so an empty bank costs the turn nothing.
	{Name: "memory-recall", Event: EventUserPromptSubmit, Timeout: 5, Vendors: []string{"codex", "copilot"}},
	{Name: "session-harvest-end", Event: EventSessionEnd, Timeout: 15},
	// Before a compaction, harvest edits no earlier dispatch covered (never blocks; the transcript survives).
	{Name: "pre-compact", Event: EventPreCompact, Timeout: 15},
	// Native worktrees live at ~/worktrees/<repo>/<name>, the same root Orca uses; these replace Claude's git logic.
	{Name: "worktree-create", Event: EventWorktreeCreate, Timeout: 60},
	{Name: "worktree-remove", Event: EventWorktreeRemove, Timeout: 30},
}

// Hook and Group are the settings.json hook schema.
type Hook struct {
	Type          string `json:"type"`
	Command       string `json:"command"`
	Timeout       int    `json:"timeout"`
	Async         bool   `json:"async,omitempty"`
	StatusMessage string `json:"statusMessage,omitempty"`
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
		h := Hook{Type: "command", Command: s.Command(), Timeout: s.Timeout, Async: s.Async, StatusMessage: s.StatusMessage}
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
