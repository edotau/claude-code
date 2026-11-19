// This file declares the harness hook chain and renders settings.json's "hooks" block.

package settings

import (
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/textutil"
)

// hookBin is the literal binary reference in harness commands; $HOME expands under `sh -c`.
const hookBin = "$HOME/.claude/bin/claude-code"

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
	Event         string
	Matcher       string        // "" = every tool / no matcher
	Budget        time.Duration // the handler's deadline and its share of the dispatcher's timeout
	Blocks        bool          // preserve blocking exits; other Copilot hooks are advisory
	Agents        []string      // explicit allowlist for non-Claude sessions
	Hosted        []string      // agents whose headless host runs the module around a run
	Plugin        []string      // agents whose in-process plugin runs the module
	StatusMessage string        // spinner text Claude Code shows while the hook runs
	Async         string        // Matcher alternatives Claude runs in the background entry, after the tool returns
	AlwaysOn      bool          // HARNESS_HOOK_SKIP=all spares it; only its own name skips it
	FailExit      bool          // a failed or overrun run exits 1, never 0
	Raw           bool          // may return a raw stdout and exit code, which the dispatcher otherwise rejects
	BlockJSON     []string      // agents that also get the JSON deny on stdout when it blocks
}

// Seconds is Budget as the whole-second timeout settings.json carries.
func (s EventSpec) Seconds() int { return int((s.Budget + time.Second - 1) / time.Second) }

// Command is the shell command settings.json carries for this module alone.
func (s EventSpec) Command() string { return hookBin + " hook " + s.Event + " --" + s.Name }

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

// DispatcherCommand is the one-per-event entry; it skips modules that render as their own entries.
func DispatcherCommand(event string) string { return hookBin + " hook " + event }

// AllFlag makes `hook <Event>` run every module on the event, own-process ones included.
const AllFlag = "--all"

// AsyncFlag makes `hook <Event>` run only each module's Async alternatives: the background entry's command.
const AsyncFlag = "--async"

// Any HARNESS_HOOK_* marker counts, so a renamed marker's stale entries are replaced on render, not kept as foreign.
var harnessCommand = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^(?:HARNESS_HOOK_[A-Z]+=\w+ )?` + regexp.QuoteMeta(hookBin) + ` `)
})

// foreignRTKHook is the bare `rtk hook claude` entry `rtk init -g` writes; the harness rtk-rewrite replaces it.
var foreignRTKHook = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^(?:\S*/)?rtk hook claude\b`) })

// IsHarness reports whether a hook command is this harness's: plain, agent-marked, or the stale foreign rtk hook.
func IsHarness(command string) bool {
	c := strings.TrimSpace(command)
	return harnessCommand().MatchString(c) || foreignRTKHook().MatchString(c)
}

const sec = time.Second

// Registry is the whole chain: groups in execution order, each naming its events.
var Registry = []Group{
	{Name: "safety", Gate: true, Events: []EventSpec{
		{Name: "safety-scan", Event: EventPreToolUse, Matcher: "Bash", Budget: 5 * sec, StatusMessage: "Checking command safety", Blocks: true, AlwaysOn: true, BlockJSON: []string{"copilot"}, Agents: []string{"codex", "copilot", "gemini"}, Hosted: []string{"opencode"}, Plugin: []string{"opencode"}},
	}},
	// Copilot has no permission-gate: its edit tools reach safety-scan for the Edit deny globs.
	{Name: "safety-edit", Gate: true, AgentOnly: true, Events: []EventSpec{
		{Name: "safety-scan", Event: EventPreToolUse, Matcher: "Write|Edit", Budget: 5 * sec, Blocks: true, AlwaysOn: true, BlockJSON: []string{"copilot"}, Agents: []string{"copilot"}},
	}},
	// AgentOnly: Claude enforces a dispatch's Permissions natively, so settings.json never renders the gate.
	{Name: "permission", Gate: true, AgentOnly: true, Events: []EventSpec{
		{Name: "permission-gate", Event: EventPreToolUse, Matcher: "Bash|Write|Edit|Read|WebFetch|Glob|Grep", Budget: 5 * sec, Blocks: true, AlwaysOn: true, Agents: []string{"codex", "gemini"}, Plugin: []string{"opencode"}},
	}},
	{Name: "rtk", Events: []EventSpec{
		// An eligible noisy Bash command is rewritten to run through the rtk output compressor (Claude only).
		{Name: "rtk-rewrite", Event: EventPreToolUse, Matcher: "Bash", Budget: 5 * sec},
	}},
	{Name: "docs", Events: []EventSpec{
		// Root or ignored docs/ markdown is blocked, a new plan-named one goes to the plans home, a draft needs the template.
		{Name: "docs-placement", Event: EventPreToolUse, Matcher: "Write|Edit|MultiEdit|Bash", Budget: 5 * sec, Blocks: true, Agents: []string{"codex", "copilot", "gemini"}, Hosted: []string{"opencode"}, Plugin: []string{"opencode"}},
		// Every write/edit to a native plan-mode draft is mirrored to its dated, repo-tagged file in plans/.
		{Name: "docs-plan-mirror", Event: EventPostToolUse, Matcher: "Write|Edit", Budget: 10 * sec, Agents: []string{"gemini"}, Hosted: []string{"opencode"}},
		// An approved plan is filed under plans/, tagged with its repo.
		{Name: "docs-plan-save", Event: EventPostToolUse, Matcher: "ExitPlanMode", Budget: 10 * sec},
		// A commit on main ticks the plan tasks its Task: trailers name; coverage alone only prints a candidate.
		{Name: "docs-plan-tick", Event: EventPostToolUse, Matcher: "Bash", Async: "Bash", Budget: 10 * sec, Agents: []string{"gemini"}, Hosted: []string{"opencode"}},
		// The Stop slot of the plan hooks; its partly-landed record is no longer written.
		{Name: "docs-plan-reminder", Event: EventStop, Budget: 5 * sec, Agents: []string{"gemini"}, Hosted: []string{"opencode"}},
	}},
	{Name: "dispatch", Events: []EventSpec{
		// One PreToolUse runner: cap, then model pin (Workflow leaves too), then the claim.
		{Name: "dispatch-effort", Event: EventPreToolUse, Matcher: "Agent|Task|Workflow", Budget: 5 * sec},
		// A never-run spawn or finished Workflow frees its slots; a relay poll that returns on an ended job: hand back.
		{Name: "dispatch-failure", Event: EventPostToolUseFailure, Matcher: "Agent|Task|Workflow", Budget: 5 * sec},
		{Name: "dispatch-workflow", Event: EventPostToolUse, Matcher: "Workflow|Bash", Budget: 5 * sec},
		// Retires the stopping subagent's slot by agent id.
		{Name: "dispatch-stop", Event: EventSubagentStop, Budget: 15 * sec},
		// Stop gates at SubagentHandback (ends a background leaf first); on Bash, a reported or second relay poll is refused.
		{Name: "dispatch-handback", Event: EventPreToolUse, Matcher: "SubagentHandback|Bash", Budget: 15 * sec},
	}},
	{Name: "paths", Events: []EventSpec{
		// A pasted path (Windows, POSIX or client-only) resolves for the model, naming images and large files.
		{Name: "paths-resolve", Event: EventUserPromptSubmit, Budget: 5 * sec, Agents: []string{"codex", "copilot"}, Hosted: []string{"opencode"}},
	}},
	{Name: "memory", Events: []EventSpec{
		// One Stop hook: context checkpoint, then the memory update; neither blocks.
		{Name: "memory-update", Event: EventStop, Budget: 20 * sec, Agents: []string{"codex", "copilot", "gemini"}, Hosted: []string{"opencode"}},
		{Name: "memory-start", Event: EventSessionStart, Budget: 10 * sec, Agents: []string{"codex", "copilot", "gemini"}, StatusMessage: "Loading session memory", Hosted: []string{"opencode"}},
		// Per-prompt bank recall: advisory and capped, so an empty bank costs the turn nothing.
		{Name: "memory-recall", Event: EventUserPromptSubmit, Budget: 5 * sec, Agents: []string{"codex", "copilot"}, Hosted: []string{"opencode"}},
		{Name: "memory-end", Event: EventSessionEnd, Budget: 15 * sec, Agents: []string{"codex", "copilot"}, Hosted: []string{"opencode"}},
		// Before a compaction, update edits no earlier dispatch covered (never blocks; the transcript survives).
		{Name: "memory-compact", Event: EventPreCompact, Budget: 90 * sec},
	}},
	{Name: "format", Events: []EventSpec{
		// Formats files dirty in git that this session wrote; a repo with its own pre-commit switches the group off.
		{Name: "format-stop", Event: EventStop, Budget: 30 * sec, Agents: []string{"copilot", "gemini"}, Hosted: []string{"opencode"}},
	}},
	{Name: "review", Events: []EventSpec{
		// Advisory comment, complexity and smell findings an edit just introduced, fed back to the model.
		{Name: "review-simplicity", Event: EventPostToolUse, Matcher: "Write|Edit", Budget: 5 * sec, Agents: []string{"gemini"}, Hosted: []string{"opencode"}},
	}},
	{Name: "worktree", Events: []EventSpec{
		// Native worktrees live at ~/worktrees/<repo>/<name>; these replace Claude's git logic.
		{Name: "worktree-create", Raw: true, FailExit: true, Event: EventWorktreeCreate, Budget: 60 * sec},
		{Name: "worktree-remove", Raw: true, FailExit: true, Event: EventWorktreeRemove, Budget: 30 * sec},
		// Ask-about-a-worktree notes (silent when HARNESS_WORKTREE=off); the busy check runs every prompt.
		{Name: "worktree-busy", Raw: true, Event: EventUserPromptSubmit, Budget: 5 * sec},
		// Each Write/Edit path (and each `git add`-staged path) appends to this session's claims ledger; never blocks.
		{Name: "worktree-claim", Event: EventPostToolUse, Matcher: "Write|Edit|MultiEdit|Bash", Async: "Bash", Budget: 5 * sec, Agents: []string{"codex", "copilot", "gemini"}, Plugin: []string{"opencode"}},
		// Before an edit, notes a dirty file another live session/leaf claims; before git add/commit, asks on one.
		{Name: "worktree-claim-check", Event: EventPreToolUse, Matcher: "Write|Edit|MultiEdit|Bash", Budget: 5 * sec, Agents: []string{"codex", "copilot", "gemini"}, Plugin: []string{"opencode"}},
	}},
	{Name: "secrets", Events: []EventSpec{
		// A Bash result holding a vault value is replaced before the model and the transcript keep it (Claude only).
		{Name: "secrets-out", Event: EventPostToolUse, Matcher: "Bash", Budget: 2 * sec},
		// A detached pass masks the ended session's transcripts in place once they sit idle (Claude only).
		{Name: "secrets-end", Event: EventSessionEnd, Budget: 2 * sec},
	}},
	{Name: "codex", AgentOnly: true, Events: []EventSpec{
		{Name: "codex-agent-route", Event: EventPreToolUse, Matcher: ".*spawn_agent$", Budget: 5 * sec, Agents: []string{"codex"}},
		{Name: "codex-routing-result", Event: EventPostToolUse, Matcher: ".*spawn_agent$", Budget: 5 * sec, Agents: []string{"codex"}},
		{Name: "codex-routing-start", Event: EventSessionStart, Budget: 5 * sec, Agents: []string{"codex"}},
		{Name: "codex-subagent-start", Event: EventSubagentStart, Budget: 5 * sec, Agents: []string{"codex"}},
	}},
}

func Specs() []EventSpec {
	var out []EventSpec
	for _, g := range Registry {
		out = append(out, g.Events...)
	}
	return out
}

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

// RunsGate reports whether this group synchronously runs command for agent on tool.
func (g MatcherGroup) RunsGate(agent, command, tool string) bool {
	if g.Matcher != "" {
		re, err := regexp.Compile("^(?:" + g.Matcher + ")$")
		if err != nil || !re.MatchString(tool) {
			return false
		}
	}
	return slices.ContainsFunc(g.Hooks, func(h Hook) bool {
		cmd := strings.TrimSpace(h.Command)
		return h.Type == "command" && !h.Async && (cmd == command || cmd == AgentEnv+"="+agent+" "+command)
	})
}

type dispatcherSlot struct {
	matchers []string
	any      bool // some handler has no matcher, so the entry carries none
	seconds  int
	status   string
	nstatus  int // modules on the event that set a status; the spinner shows only when exactly one does
	pos      int
	async    bool
}

func (d *dispatcherSlot) add(s EventSpec) {
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
	if d.async {
		mg.Hooks[0].Command, mg.Hooks[0].Async = mg.Hooks[0].Command+" "+AsyncFlag, true
	}
	if !d.any {
		mg.Matcher = strings.Join(d.matchers, "|")
	}
	if d.nstatus == 1 {
		mg.Hooks[0].StatusMessage = d.status
	}
	return mg
}

// asyncSplit is s's synchronous part (Matcher less its Async alternatives) and its async part; empty parts drop.
func asyncSplit(s EventSpec) []EventSpec {
	if s.Async == "" {
		return []EventSpec{s}
	}
	async := strings.Split(s.Async, "|")
	var keep []string
	for alt := range strings.SplitSeq(s.Matcher, "|") {
		if !slices.Contains(async, alt) {
			keep = append(keep, alt)
		}
	}
	fg, bg := s, s
	fg.Matcher, fg.Async, bg.Matcher = strings.Join(keep, "|"), "", s.Async
	if len(keep) == 0 {
		return []EventSpec{bg}
	}
	return []EventSpec{fg, bg}
}

// slotKey keys a dispatcher entry: one synchronous and one async (background) entry per event at most.
type slotKey struct {
	event string
	async bool
}

// Events renders a synchronous and an async dispatcher per event as needed, plus separate gate entries.
func Events() map[string][]MatcherGroup {
	block := map[string][]MatcherGroup{}
	disp := map[slotKey]*dispatcherSlot{}
	for _, g := range Registry {
		if g.AgentOnly {
			continue
		}
		for _, s := range g.Events {
			if g.Gate {
				h := Hook{Type: "command", Command: s.Command(), Timeout: s.Seconds(), StatusMessage: s.StatusMessage}
				block[s.Event] = append(block[s.Event], MatcherGroup{Matcher: s.Matcher, Hooks: []Hook{h}})
				continue
			}
			for _, p := range asyncSplit(s) {
				k := slotKey{s.Event, p.Async != ""}
				d := disp[k]
				if d == nil {
					d = &dispatcherSlot{pos: len(block[s.Event]), async: k.async}
					disp[k] = d
					block[s.Event] = append(block[s.Event], MatcherGroup{})
				}
				d.add(p)
			}
		}
	}
	for k, d := range disp {
		block[k.event][d.pos] = d.group(k.event)
	}
	return block
}

func settingsJSON() ([]byte, error) { return textutil.MarshalIndentNoEscape(Events()) }

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
	var names []string
	for _, sp := range Specs() {
		names = append(names, sp.Name)
	}
	return HookHelp("modules: ", names)
}

// HookHelp wraps hook names into help lines under 80 columns.
func HookHelp(prefix string, names []string) string {
	var lines []string
	line := prefix
	for _, name := range names {
		if len(line)+len(name)+1 > 79 {
			lines = append(lines, line)
			line = "         "
		}
		line += name + "|"
	}
	return strings.Join(append(lines, strings.TrimSuffix(line, "|")), "\n")
}

// SpecsFor lists the specs agentName runs on a surface: native, hosted or plugin; unknown combinations are nil.
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
	case "hosted":
		return slices.Contains(s.Hosted, agentName)
	case "plugin":
		return slices.Contains(s.Plugin, agentName)
	}
	return false
}
