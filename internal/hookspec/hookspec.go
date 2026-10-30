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

// EventSpec declares one hook module on one event; Name is its `claude-code hook <Event> --<Name>` flag.
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

// Command is the shell command settings.json carries for this module alone.
func (s EventSpec) Command() string { return HookBin + " hook " + s.Event + " --" + s.Name }

// Group is a module of hook handlers. Gate keeps it in its own process, so no other group can hang or crash it.
type Group struct {
	Name       string
	Events     []EventSpec
	Gate       bool
	VendorOnly bool // omitted from Claude's hook chain
}

// own reports whether a handler renders as its own settings entry instead of joining the event dispatcher.
func (g Group) own(s EventSpec) bool { return g.Gate || s.Async }

// DispatcherCommand is the one-per-event entry; it skips modules that render as their own entries.
func DispatcherCommand(event string) string { return HookBin + " hook " + event }

// AllFlag makes `hook <Event>` run every module on the event, own-process ones included.
const AllFlag = "--all"

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
		{Name: "safety-scan", Event: EventPreToolUse, Matcher: "Bash", Budget: 5 * sec, StatusMessage: "Checking command safety", Blocks: true, Vendors: []string{"codex", "copilot"}},
	}},
	{Name: "rtk", Events: []EventSpec{
		// An eligible noisy Bash command is rewritten to run through the rtk output compressor (Claude only).
		{Name: "rtk-rewrite", Event: EventPreToolUse, Matcher: "Bash", Budget: 5 * sec},
	}},
	{Name: "docs", Events: []EventSpec{
		// Markdown writes at a repo root or into an ignored docs/ page are blocked, naming the canonical home.
		{Name: "docs-placement", Event: EventPreToolUse, Matcher: "Write|Edit", Budget: 5 * sec, StatusMessage: "Checking doc placement", Blocks: true, Vendors: []string{"codex", "copilot"}},
		// The first write to a native plan draft must carry the Plan.md template; the denial pastes the skeleton.
		{Name: "docs-plan-gate", Event: EventPreToolUse, Matcher: "Write|Edit", Budget: 5 * sec, StatusMessage: "Checking plan template", Blocks: true, Vendors: []string{"codex", "copilot"}},
		// Every write/edit to a native plan-mode draft is mirrored to its dated, repo-tagged file in plans/.
		{Name: "docs-plan-mirror", Event: EventPostToolUse, Matcher: "Write|Edit", Budget: 10 * sec},
		// An approved plan is filed under plans/, tagged with its repo.
		{Name: "docs-plan-save", Event: EventPostToolUse, Matcher: "ExitPlanMode", Budget: 10 * sec},
		// The Plan.md skeleton is injected on entering plan mode and, once per plan, on a plan-mode prompt.
		{Name: "docs-plan-skeleton", Event: EventPostToolUse, Matcher: "EnterPlanMode", Budget: 5 * sec},
		{Name: "docs-plan-skeleton-prompt", Event: EventUserPromptSubmit, Budget: 5 * sec},
		// A landed git commit ticks the active plan's tasks whose owns: it covers; partial coverage is noted.
		{Name: "docs-plan-tick", Event: EventPostToolUse, Matcher: "Bash", Budget: 10 * sec},
		// Commits that only partly landed a task are reported once at Stop (advisory).
		{Name: "docs-plan-reminder", Event: EventStop, Budget: 5 * sec},
	}},
	{Name: "dispatch", Events: []EventSpec{
		// One PreToolUse runner: cap, then model pin (Workflow leaves too), then the claim.
		{Name: "dispatch-effort", Event: EventPreToolUse, Matcher: "Agent|Task|Workflow", Budget: 5 * sec},
		// A spawn that never ran (PostToolUseFailure) or a finished Workflow releases its tool_use_id's slots.
		{Name: "dispatch-failure", Event: EventPostToolUseFailure, Matcher: "Agent|Task|Workflow", Budget: 5 * sec},
		{Name: "dispatch-workflow", Event: EventPostToolUse, Matcher: "Workflow", Budget: 5 * sec},
		// Retires the stopping subagent's slot by agent id.
		{Name: "dispatch-stop", Event: EventSubagentStop, Budget: 15 * sec},
	}},
	{Name: "paths", Events: []EventSpec{
		// A pasted or dragged Windows path (C:\…) resolves to its /mnt form for the model; a no-op off WSL.
		{Name: "paths-resolve", Event: EventUserPromptSubmit, Budget: 5 * sec, Vendors: []string{"codex", "copilot"}},
	}},
	{Name: "memory", Events: []EventSpec{
		// One Stop hook: context checkpoint, then the memory harvest; a block from either blocks.
		{Name: "memory-harvest", Event: EventStop, Budget: 20 * sec},
		{Name: "memory-start", Event: EventSessionStart, Budget: 10 * sec, StatusMessage: "Loading session memory"},
		// Per-prompt bank recall: advisory and capped, so an empty bank costs the turn nothing.
		{Name: "memory-recall", Event: EventUserPromptSubmit, Budget: 5 * sec, Vendors: []string{"codex", "copilot"}},
		{Name: "memory-end", Event: EventSessionEnd, Budget: 15 * sec},
		// Before a compaction, harvest edits no earlier dispatch covered (never blocks; the transcript survives).
		{Name: "memory-compact", Event: EventPreCompact, Budget: 15 * sec},
	}},
	{Name: "format", Events: []EventSpec{
		// Formats files dirty in git that this session wrote; a repo with its own pre-commit switches the group off.
		{Name: "format-stop", Event: EventStop, Budget: 30 * sec},
	}},
	{Name: "worktree", Events: []EventSpec{
		// Native worktrees live at ~/worktrees/<repo>/<name>, the same root Orca uses; these replace Claude's git logic.
		{Name: "worktree-create", Event: EventWorktreeCreate, Budget: 60 * sec},
		{Name: "worktree-remove", Event: EventWorktreeRemove, Budget: 30 * sec},
		// Ask-about-a-worktree notes (HARNESS_WORKTREE); the busy check runs every prompt, so it stays cheap.
		{Name: "worktree-mode", Event: EventSessionStart, Budget: 5 * sec},
		{Name: "worktree-busy", Event: EventUserPromptSubmit, Budget: 5 * sec},
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

// GroupOf names the group a hook module belongs to.
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
		nstatus  int // modules on the event that set a status; the spinner shows only when exactly one does
		pos      int
	}
	disp := map[string]*slot{}
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
			if s.StatusMessage != "" {
				d.status, d.nstatus = s.StatusMessage, d.nstatus+1
			}
			if s.Matcher == "" {
				d.any = true
			} else if !slices.Contains(d.matchers, s.Matcher) {
				d.matchers = append(d.matchers, s.Matcher)
			}
		}
	}
	for event, d := range disp {
		mg := MatcherGroup{Hooks: []Hook{{Type: "command", Command: DispatcherCommand(event), Timeout: d.seconds}}}
		if !d.any {
			mg.Matcher = strings.Join(d.matchers, "|")
		}
		if d.nstatus == 1 {
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
