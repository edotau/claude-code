// Package hookspec declares the harness hook chain and renders settings.json's "hooks" block.
package hookspec

import (
	"bytes"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"time"
)

// HookBin is the literal binary reference in harness commands; $HOME expands under `sh -c`.
const HookBin = "$HOME/.claude/bin/claude-code"

// AgentEnv identifies the agent invoking a hook with a shared payload contract.
const AgentEnv = "HARNESS_HOOK_AGENT"

// CodexProfileEnv identifies the inherited provider for native Codex child routing.
const CodexProfileEnv = "HARNESS_CODEX_PROFILE"

// CodexCatalogEnv identifies the validated catalog supplied to the inherited Codex session.
const CodexCatalogEnv = "HARNESS_CODEX_CATALOG"

// Claude Code lifecycle event names.
const (
	EventPreToolUse         = "PreToolUse"
	EventPostToolUse        = "PostToolUse"
	EventPostToolUseFailure = "PostToolUseFailure"
	EventSubagentStart      = "SubagentStart"
	EventSubagentStop       = "SubagentStop"
	EventSessionStart       = "SessionStart"
	EventUserPromptSubmit   = "UserPromptSubmit"
	EventStop               = "Stop"
	EventSessionEnd         = "SessionEnd"
	EventPreCompact         = "PreCompact"
	EventWorktreeCreate     = "WorktreeCreate"
	EventWorktreeRemove     = "WorktreeRemove"
)

// EventSpec declares one hook handler on one event; Name is its `claude-code hook <Name>` verb.
type EventSpec struct {
	Name          string
	Group         string // set by Specs
	Event         string
	Matcher       string        // "" = every tool / no matcher
	Budget        time.Duration // the handler's deadline and its share of the dispatcher's timeout
	Blocks        bool          // preserve blocking exits; other Copilot hooks are advisory
	Vendors       []string      // explicit allowlist for non-Claude sessions
	Async         bool          // runs in the background, so it renders as its own entry, never in the dispatcher
	StatusMessage string        // spinner text Claude Code shows while the hook runs
}

// Seconds is Budget as the whole-second timeout settings.json carries.
func (s EventSpec) Seconds() int { return int((s.Budget + time.Second - 1) / time.Second) }

// Command is the shell command settings.json carries for this handler alone.
func (s EventSpec) Command() string { return HookBin + " hook " + s.Name }

// Group is a module of hook handlers. Gate keeps it in its own process, so no other group can hang or crash it.
type Group struct {
	Name       string
	Events     []EventSpec
	Gate       bool
	VendorOnly bool // omitted from Claude's hook chain
}

// own reports whether a handler renders as its own settings entry instead of joining the event dispatcher.
func (g Group) own(s EventSpec) bool { return g.Gate || s.Async }

// DispatcherCommand is the one-per-event entry; noOwn skips handlers that render as their own entries.
func DispatcherCommand(event string, noOwn bool) string {
	c := HookBin + " hook " + event
	if noOwn {
		c += " " + NoOwnFlag
	}
	return c
}

// NoOwnFlag makes the dispatcher skip the gate groups and async handlers that settings.json runs separately.
const NoOwnFlag = "--no-own"

// Any HARNESS_HOOK_* marker counts, so a renamed marker's stale entries are replaced on render, not kept as foreign.
var harnessCommand = regexp.MustCompile(`^(?:HARNESS_HOOK_[A-Z]+=\w+ )?` + regexp.QuoteMeta(HookBin) + ` `)

// IsHarness reports whether a hook command belongs to this harness, including vendor-marked commands.
func IsHarness(command string) bool {
	return harnessCommand.MatchString(strings.TrimSpace(command))
}

const sec = time.Second

// Registry is the whole chain: groups in execution order, each naming its events.
var Registry = []Group{
	{Name: "safety", Gate: true, Events: []EventSpec{
		{Name: "bash-guard", Event: EventPreToolUse, Matcher: "Bash", Budget: 5 * sec, StatusMessage: "Checking command safety", Blocks: true, Vendors: []string{"codex", "copilot"}},
	}},
	{Name: "plans", Events: []EventSpec{
		// Check markdown placement, then require template headings for a native plan draft's first write.
		{Name: "docs-placement", Event: EventPreToolUse, Matcher: "Write|Edit", Budget: 5 * sec, StatusMessage: "Checking doc placement", Blocks: true, Vendors: []string{"codex", "copilot"}},
		// Every write/edit to a native plan-mode draft is mirrored to its dated, repo-tagged file in plans/.
		{Name: "plan-mirror", Event: EventPostToolUse, Matcher: "Write|Edit", Budget: 10 * sec},
		// An approved plan is filed under plans/, tagged with its repo.
		{Name: "save-plan", Event: EventPostToolUse, Matcher: "ExitPlanMode", Budget: 10 * sec},
	}},
	{Name: "dispatch", Events: []EventSpec{
		// An unset dispatch model is filled from the local prompt-complexity classifier (low→haiku, else sonnet).
		{Name: "agent-effort", Event: EventPreToolUse, Matcher: "Agent|Task", Budget: 5 * sec},
		// An inline Workflow script's unpinned agent() leaves are pinned to sonnet (else they inherit the session model).
		{Name: "workflow-lint", Event: EventPreToolUse, Matcher: "Workflow", Budget: 5 * sec},
		// Machine-wide subagent cap: pre counts and blocks, tag binds the marker to the agent id, post retires it.
		{Name: "agent-inflight", Event: EventPreToolUse, Matcher: "Agent|Task", Budget: 5 * sec},
		{Name: "agent-inflight-tag", Event: EventPostToolUse, Matcher: "Agent|Task", Budget: 5 * sec},
		{Name: "agent-inflight-post", Event: EventPostToolUseFailure, Matcher: "Agent|Task", Budget: 5 * sec},
		// Retires the stopping subagent's inflight slot on the stop that actually ends it.
		{Name: "subagent-stop", Event: EventSubagentStop, Budget: 15 * sec},
	}},
	{Name: "paths", Events: []EventSpec{
		// A pasted or dragged Windows path (C:\…) resolves to its /mnt form for the model; a no-op off WSL.
		{Name: "path-resolver", Event: EventUserPromptSubmit, Budget: 5 * sec, Vendors: []string{"codex", "copilot"}},
	}},
	{Name: "memory", Events: []EventSpec{
		// A model read of a bank file is traced to that bank's access log (memory review's hot/cold signal).
		{Name: "access-trace", Event: EventPostToolUse, Matcher: "Read", Budget: 5 * sec, Async: true, Vendors: []string{"codex", "copilot"}},
		// One Stop hook: context checkpoint, memory harvest, then the formatter; a block from the first two blocks.
		{Name: "stop", Event: EventStop, Budget: 50 * sec},
		{Name: "session-start", Event: EventSessionStart, Budget: 10 * sec, StatusMessage: "Loading session memory"},
		// Per-prompt bank recall: advisory and capped, so an empty bank costs the turn nothing.
		{Name: "memory-recall", Event: EventUserPromptSubmit, Budget: 5 * sec, Vendors: []string{"codex", "copilot"}},
		{Name: "session-harvest-end", Event: EventSessionEnd, Budget: 15 * sec},
		// Before a compaction, harvest edits no earlier dispatch covered (never blocks; the transcript survives).
		{Name: "pre-compact", Event: EventPreCompact, Budget: 15 * sec},
	}},
	{Name: "worktree", Events: []EventSpec{
		// Native worktrees live at ~/worktrees/<repo>/<name>, the same root Orca uses; these replace Claude's git logic.
		{Name: "worktree-create", Event: EventWorktreeCreate, Budget: 60 * sec},
		{Name: "worktree-remove", Event: EventWorktreeRemove, Budget: 30 * sec},
	}},
	{Name: "codex", VendorOnly: true, Events: []EventSpec{
		{Name: "codex-agent-route", Event: EventPreToolUse, Matcher: ".*spawn_agent$", Budget: 5 * sec, Vendors: []string{"codex"}},
		{Name: "codex-routing-result", Event: EventPostToolUse, Matcher: ".*spawn_agent$", Budget: 5 * sec, Vendors: []string{"codex"}},
		{Name: "codex-routing-start", Event: EventSessionStart, Budget: 5 * sec, Vendors: []string{"codex"}},
		{Name: "codex-subagent-start", Event: EventSubagentStart, Budget: 5 * sec, Vendors: []string{"codex"}},
	}},
}

// Specs flattens Registry to its handlers in group order, each stamped with its group name.
func Specs() []EventSpec {
	var out []EventSpec
	for _, g := range Registry {
		for _, s := range g.Events {
			s.Group = g.Name
			out = append(out, s)
		}
	}
	return out
}

// GroupOf names the group a hook verb belongs to.
func GroupOf(name string) (Group, bool) {
	for _, g := range Registry {
		for _, s := range g.Events {
			if s.Name == name {
				return g, true
			}
		}
	}
	return Group{}, false
}

// Hook and MatcherGroup are the settings.json hook schema.
type Hook struct {
	Type          string `json:"type"`
	Command       string `json:"command"`
	Timeout       int    `json:"timeout"`
	Async         bool   `json:"async,omitempty"`
	StatusMessage string `json:"statusMessage,omitempty"`
}

type MatcherGroup struct {
	Matcher string `json:"matcher,omitempty"`
	Hooks   []Hook `json:"hooks"`
}

// Events renders the registry as event → entries: one dispatcher entry per event carrying the union of its groups'
// matchers, plus one entry each for a gate group's handlers and async handlers.
func Events() map[string][]MatcherGroup {
	block := map[string][]MatcherGroup{}
	type slot struct {
		matchers []string
		any      bool // some handler has no matcher, so the entry carries none
		seconds  int
		n        int
		status   string
		pos      int
	}
	disp := map[string]*slot{}
	hasOwn := map[string]bool{}
	for _, g := range Registry {
		if g.VendorOnly {
			continue
		}
		for _, s := range g.Events {
			if g.own(s) {
				hasOwn[s.Event] = true
			}
		}
	}
	for _, g := range Registry {
		if g.VendorOnly {
			continue
		}
		for _, s := range g.Events {
			if g.own(s) {
				h := Hook{Type: "command", Command: s.Command(), Timeout: s.Seconds(), Async: s.Async, StatusMessage: s.StatusMessage}
				block[s.Event] = append(block[s.Event], MatcherGroup{Matcher: s.Matcher, Hooks: []Hook{h}})
				continue
			}
			d := disp[s.Event]
			if d == nil {
				d = &slot{pos: len(block[s.Event])}
				disp[s.Event] = d
				block[s.Event] = append(block[s.Event], MatcherGroup{})
			}
			d.n++
			d.seconds += s.Seconds()
			d.status = s.StatusMessage
			if s.Matcher == "" {
				d.any = true
			} else if !slices.Contains(d.matchers, s.Matcher) {
				d.matchers = append(d.matchers, s.Matcher)
			}
		}
	}
	for event, d := range disp {
		mg := MatcherGroup{Hooks: []Hook{{Type: "command", Command: DispatcherCommand(event, hasOwn[event]), Timeout: d.seconds}}}
		if !d.any {
			mg.Matcher = strings.Join(d.matchers, "|")
		}
		if d.n == 1 {
			mg.Hooks[0].StatusMessage = d.status
		}
		block[event][d.pos] = mg
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
