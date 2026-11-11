// This file declares the harness hook chain and renders settings.json's "hooks" block.

package settings

import (
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/textutil"
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
	Agents        []string      // explicit allowlist for non-Claude sessions
	Hosted        []string      // agents whose headless host runs the module around a run
	Plugin        []string      // agents whose in-process plugin runs the module
	Async         bool          // runs in the background, so it renders as its own entry, never in the dispatcher
	StatusMessage string        // spinner text Claude Code shows while the hook runs
}

// Seconds is Budget as the whole-second timeout settings.json carries.
func (s EventSpec) Seconds() int { return int((s.Budget + time.Second - 1) / time.Second) }

// Command is the shell command settings.json carries for this module alone.
func (s EventSpec) Command() string { return HookBin + " hook " + s.Event + " --" + s.Name }

// AgentCommand is s.Command() prefixed with AgentEnv=agent, as a non-Claude agent's rendered config carries it.
func AgentCommand(agent string, s EventSpec) string {
	return AgentEnv + "=" + agent + " " + s.Command()
}

// Group is a module of hook handlers. Gate keeps it in its own process, so no other group can hang or crash it.
type Group struct {
	Name      string
	Events    []EventSpec
	Gate      bool
	AgentOnly bool // omitted from Claude's hook chain
}

// own reports whether a handler renders as its own settings entry instead of joining the event dispatcher.
func (g Group) own(s EventSpec) bool { return g.Gate || s.Async }

// DispatcherCommand is the one-per-event entry; it skips modules that render as their own entries.
func DispatcherCommand(event string) string { return HookBin + " hook " + event }

// AllFlag makes `hook <Event>` run every module on the event, own-process ones included.
const AllFlag = "--all"

// Any HARNESS_HOOK_* marker counts, so a renamed marker's stale entries are replaced on render, not kept as foreign.
var harnessCommand = regexp.MustCompile(`^(?:HARNESS_HOOK_[A-Z]+=\w+ )?` + regexp.QuoteMeta(HookBin) + ` `)

// foreignRTKHook is the bare `rtk hook claude` entry `rtk init -g` writes; the harness rtk-rewrite replaces it.
var foreignRTKHook = regexp.MustCompile(`^(?:\S*/)?rtk hook claude\b`)

// IsHarness reports whether a hook command belongs to this harness, including agent-marked commands and the stale foreign rtk hook.
func IsHarness(command string) bool {
	return harnessCommand.MatchString(strings.TrimSpace(command)) || foreignRTKHook.MatchString(strings.TrimSpace(command))
}

const sec = time.Second

// Registry is the whole chain: groups in execution order, each naming its events.
var Registry = []Group{
	{Name: "safety", Gate: true, Events: []EventSpec{
		{Name: "safety-scan", Event: EventPreToolUse, Matcher: "Bash", Budget: 5 * sec, StatusMessage: "Checking command safety", Blocks: true, Agents: []string{"codex", "copilot"}, Hosted: []string{"opencode"}, Plugin: []string{"opencode"}},
	}},
	// AgentOnly: Claude enforces a dispatch's Permissions natively, so settings.json never renders the gate.
	{Name: "permission", Gate: true, AgentOnly: true, Events: []EventSpec{
		{Name: "permission-gate", Event: EventPreToolUse, Matcher: "Bash|Write|Edit|Read|WebFetch|Glob|Grep", Budget: 5 * sec, Blocks: true, Agents: []string{"codex", "gemini"}, Plugin: []string{"opencode"}},
	}},
	{Name: "rtk", Events: []EventSpec{
		// An eligible noisy Bash command is rewritten to run through the rtk output compressor (Claude only).
		{Name: "rtk-rewrite", Event: EventPreToolUse, Matcher: "Bash", Budget: 5 * sec},
	}},
	{Name: "docs", Events: []EventSpec{
		// Markdown writes at a repo root or into an ignored docs/ page are blocked, naming the canonical home.
		{Name: "docs-placement", Event: EventPreToolUse, Matcher: "Write|Edit", Budget: 5 * sec, StatusMessage: "Checking doc placement", Blocks: true, Agents: []string{"codex", "copilot"}, Hosted: []string{"opencode"}, Plugin: []string{"opencode"}},
		// The first write to a native plan draft must carry the plan template; the denial pastes the skeleton.
		{Name: "docs-plan-gate", Event: EventPreToolUse, Matcher: "Write|Edit", Budget: 5 * sec, StatusMessage: "Checking plan template", Blocks: true, Agents: []string{"codex", "copilot"}, Hosted: []string{"opencode"}, Plugin: []string{"opencode"}},
		// Every write/edit to a native plan-mode draft is mirrored to its dated, repo-tagged file in plans/.
		{Name: "docs-plan-mirror", Event: EventPostToolUse, Matcher: "Write|Edit", Budget: 10 * sec, Hosted: []string{"opencode"}},
		// An approved plan is filed under plans/, tagged with its repo.
		{Name: "docs-plan-save", Event: EventPostToolUse, Matcher: "ExitPlanMode", Budget: 10 * sec},
		// The plan template is injected on entering plan mode and, once per plan, on a plan-mode prompt.
		{Name: "docs-plan-skeleton", Event: EventPostToolUse, Matcher: "EnterPlanMode", Budget: 5 * sec},
		{Name: "docs-plan-skeleton-prompt", Event: EventUserPromptSubmit, Budget: 5 * sec, Hosted: []string{"opencode"}},
		// A landed git commit ticks the active plan's tasks whose owns: it covers; partial coverage is noted.
		{Name: "docs-plan-tick", Event: EventPostToolUse, Matcher: "Bash", Budget: 10 * sec, Hosted: []string{"opencode"}},
		// Commits that only partly landed a task are reported once at Stop (advisory).
		{Name: "docs-plan-reminder", Event: EventStop, Budget: 5 * sec, Hosted: []string{"opencode"}},
	}},
	{Name: "dispatch", Events: []EventSpec{
		// One PreToolUse runner: cap, then model pin (Workflow leaves too), then the claim.
		{Name: "dispatch-effort", Event: EventPreToolUse, Matcher: "Agent|Task|Workflow", Budget: 5 * sec},
		// A spawn that never ran (PostToolUseFailure) or a finished Workflow releases its tool_use_id's slots.
		{Name: "dispatch-failure", Event: EventPostToolUseFailure, Matcher: "Agent|Task|Workflow", Budget: 5 * sec},
		{Name: "dispatch-workflow", Event: EventPostToolUse, Matcher: "Workflow", Budget: 5 * sec},
		// Retires the stopping subagent's slot by agent id.
		{Name: "dispatch-stop", Event: EventSubagentStop, Budget: 15 * sec},
		// A background leaf ends at SubagentHandback, before SubagentStop: the same stop gates run there.
		{Name: "dispatch-handback", Event: EventPreToolUse, Matcher: "SubagentHandback", Budget: 5 * sec},
	}},
	{Name: "paths", Events: []EventSpec{
		// A pasted or dragged Windows path (C:\…) resolves to its /mnt form for the model; a no-op off WSL.
		{Name: "paths-resolve", Event: EventUserPromptSubmit, Budget: 5 * sec, Agents: []string{"codex", "copilot"}, Hosted: []string{"opencode"}},
	}},
	{Name: "memory", Events: []EventSpec{
		// One Stop hook: context checkpoint, then the memory update; a block from either blocks.
		{Name: "memory-update", Event: EventStop, Budget: 20 * sec, Agents: []string{"codex", "copilot"}, Hosted: []string{"opencode"}},
		{Name: "memory-start", Event: EventSessionStart, Budget: 10 * sec, StatusMessage: "Loading session memory", Hosted: []string{"opencode"}},
		// Per-prompt bank recall: advisory and capped, so an empty bank costs the turn nothing.
		{Name: "memory-recall", Event: EventUserPromptSubmit, Budget: 5 * sec, Agents: []string{"codex", "copilot"}, Hosted: []string{"opencode"}},
		{Name: "memory-end", Event: EventSessionEnd, Budget: 15 * sec, Agents: []string{"codex", "copilot"}, Hosted: []string{"opencode"}},
		// Before a compaction, update edits no earlier dispatch covered (never blocks; the transcript survives).
		{Name: "memory-compact", Event: EventPreCompact, Budget: 15 * sec},
	}},
	{Name: "format", Events: []EventSpec{
		// Formats files dirty in git that this session wrote; a repo with its own pre-commit switches the group off.
		{Name: "format-stop", Event: EventStop, Budget: 30 * sec, Agents: []string{"copilot"}, Hosted: []string{"opencode"}},
	}},
	{Name: "review", Events: []EventSpec{
		// Advisory comment, complexity and smell findings an edit just introduced, fed back to the model.
		{Name: "review-simplicity", Event: EventPostToolUse, Matcher: "Write|Edit", Budget: 5 * sec, Hosted: []string{"opencode"}},
	}},
	{Name: "worktree", Events: []EventSpec{
		// Native worktrees live at ~/worktrees/<repo>/<name>; these replace Claude's git logic.
		{Name: "worktree-create", Event: EventWorktreeCreate, Budget: 60 * sec},
		{Name: "worktree-remove", Event: EventWorktreeRemove, Budget: 30 * sec},
		// Ask-about-a-worktree notes (silent when HARNESS_WORKTREE=off); the busy check runs every prompt, so it stays cheap.
		{Name: "worktree-mode", Event: EventSessionStart, Budget: 5 * sec},
		{Name: "worktree-busy", Event: EventUserPromptSubmit, Budget: 5 * sec},
		// Each Write/Edit path (and each landed commit sha) appends to this session's claims ledger; never blocks.
		{Name: "worktree-claim", Event: EventPostToolUse, Matcher: "Write|Edit|Bash", Budget: 5 * sec},
		// Before an edit, notes a dirty file another live session/leaf claims; before git add/commit, asks on one.
		{Name: "worktree-claim-check", Event: EventPreToolUse, Matcher: "Write|Edit|Bash", Budget: 5 * sec, Plugin: []string{"opencode"}},
	}},
	{Name: "codex", AgentOnly: true, Events: []EventSpec{
		{Name: "codex-agent-route", Event: EventPreToolUse, Matcher: ".*spawn_agent$", Budget: 5 * sec, Agents: []string{"codex"}},
		{Name: "codex-routing-result", Event: EventPostToolUse, Matcher: ".*spawn_agent$", Budget: 5 * sec, Agents: []string{"codex"}},
		{Name: "codex-routing-start", Event: EventSessionStart, Budget: 5 * sec, Agents: []string{"codex"}},
		{Name: "codex-subagent-start", Event: EventSubagentStart, Budget: 5 * sec, Agents: []string{"codex"}},
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

type dispatcherSlot struct {
	matchers []string
	any      bool // some handler has no matcher, so the entry carries none
	seconds  int
	n        int
	status   string
	nstatus  int // modules on the event that set a status; the spinner shows only when exactly one does
	pos      int
}

func (d *dispatcherSlot) add(s EventSpec) {
	d.n++
	d.seconds += s.Seconds()
	if s.StatusMessage != "" {
		d.status, d.nstatus = s.StatusMessage, d.nstatus+1
	}
	if s.Matcher == "" {
		d.any = true
		return
	}
	for alt := range strings.SplitSeq(s.Matcher, "|") { // one alternative once, however many modules name it
		if !slices.Contains(d.matchers, alt) {
			d.matchers = append(d.matchers, alt)
		}
	}
}

func (d *dispatcherSlot) group(event string) MatcherGroup {
	mg := MatcherGroup{Hooks: []Hook{{Type: "command", Command: DispatcherCommand(event), Timeout: d.seconds}}}
	if !d.any {
		mg.Matcher = strings.Join(d.matchers, "|")
	}
	if d.nstatus == 1 {
		mg.Hooks[0].StatusMessage = d.status
	}
	return mg
}

// Events renders the registry as event → entries: one dispatcher entry per event carrying the union of its groups'
// matchers, plus one entry each for a gate group's handlers and async handlers.
func Events() map[string][]MatcherGroup {
	block := map[string][]MatcherGroup{}
	disp := map[string]*dispatcherSlot{}
	for _, g := range Registry {
		if g.AgentOnly {
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
				d = &dispatcherSlot{pos: len(block[s.Event])}
				disp[s.Event] = d
				block[s.Event] = append(block[s.Event], MatcherGroup{})
			}
			d.add(s)
		}
	}
	for event, d := range disp {
		block[event][d.pos] = d.group(event)
	}
	return block
}

// SettingsJSON renders the object under settings.json's "hooks" key.
func SettingsJSON() ([]byte, error) { return textutil.MarshalIndentNoEscape(Events()) }

// HookEvents derives from the hook chain the registry declares, in declaration order, deduplicated.
func HookEvents() []string {
	var out []string
	for _, s := range Specs() {
		if !slices.Contains(out, s.Event) {
			out = append(out, s.Event)
		}
	}
	return out
}

// HookModules wraps the module names from Specs into help lines under 80 columns.
func HookModules() string {
	var lines []string
	line := "modules: "
	for _, sp := range Specs() {
		if len(line)+len(sp.Name)+1 > 79 {
			lines = append(lines, line)
			line = "         "
		}
		line += sp.Name + "|"
	}
	return strings.Join(append(lines, strings.TrimSuffix(line, "|")), "\n")
}

// SpecsFor lists the specs agentName runs on a surface: native, sdk, hosted or plugin; unknown combinations are nil.
func SpecsFor(agentName, surface string) []EventSpec {
	var out []EventSpec
	for _, g := range Registry {
		for _, s := range g.Events {
			if specApplies(s, g.AgentOnly, agentName, surface) {
				out = append(out, s)
			}
		}
	}
	return out
}

// specApplies reports whether s runs for agentName on surface; claude skips AgentOnly groups, like Events.
func specApplies(s EventSpec, agentOnly bool, agentName, surface string) bool {
	switch surface {
	case "native":
		return agentName == "claude" && !agentOnly || slices.Contains(s.Agents, agentName)
	case "sdk":
		return agentName == "copilot" && slices.Contains(s.Agents, agentName)
	case "hosted":
		return slices.Contains(s.Hosted, agentName)
	case "plugin":
		return slices.Contains(s.Plugin, agentName)
	}
	return false
}
