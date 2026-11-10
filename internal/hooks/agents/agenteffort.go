// `agent-effort` — PreToolUse on a subagent spawn (Claude Code's Agent/Task tool) or a Workflow: fills an UNSET
// dispatch model from the local prompt classifier (low->haiku, medium/high->sonnet). Never overrides
// an explicit model/pin; fails OPEN on any error.
package agents

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"

	agentpkg "github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hooks/memhook"
	"github.com/edotau/claude-code/internal/hooks/quality"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/transcript"
)

// labelAgents are the agents a dispatch label may name, and the subagent types that carry one.
var (
	labelAgents = map[string]bool{"claude": true, "codex": true, "copilot": true, "gemini": true, "opencode": true}
	agentLineRe = regexp.MustCompile(`(?m)^[ \t]*Agent:[ \t]*(.+)$`)
	agentNameRe = regexp.MustCompile(`[a-z]+`)
)

// agentEffortResult is the pin step as a Result: a spawn gets one UpdatedInput (model pin, recall block, label);
// a Workflow gets the former workflow-lint rewrite or a Block.
func agentEffortResult(in *core.Input) core.Result {
	if in.ToolName == "Workflow" {
		return quality.WorkflowResult(in)
	}
	if !core.SpawnTools[in.ToolName] {
		return core.Result{}
	}
	// updatedInput REPLACES the whole tool_input record, so every original key must be echoed back;
	// dropping one (isolation, run_in_background, subagent_type, …) would silently alter the dispatch.
	updated := in.CloneToolInput()
	if updated == nil {
		return core.Result{}
	}

	promptField, task := spawnTask(in)

	// auto mode's classifier returns no verdict on a rewritten call, so non-functional edits ride additionalContext
	auto := in.Nested("permission_mode") == "auto"
	e := &effortPlan{updated: updated}
	recallNote := e.recall(in, promptField, task, auto)
	e.note, e.systemMessage = pinModel(in, updated, task)
	e.changed = e.changed || e.note != ""
	if deny := e.profile(promptField, task); deny != nil {
		return *deny
	}
	if deny := e.label(in, task, auto); deny != nil {
		return *deny
	}
	e.discovery(in, task, auto)
	e.note = strings.TrimSpace(e.note + "\n" + recallNote)
	return e.result()
}

// spawnTask is the spawn's prompt, or its description when the prompt is empty, with the field it came from.
func spawnTask(in *core.Input) (field, task string) {
	if task = in.Nested("tool_input", "prompt"); task != "" {
		return "prompt", task
	}
	return "description", in.Nested("tool_input", "description")
}

// effortPlan accumulates agentEffortResult's rewrite of one spawn: the new tool_input and the notes to surface.
type effortPlan struct {
	updated       map[string]any
	note          string
	systemMessage string
	changed       bool
}

// recall appends the memory-recall block to the prompt, or returns it as a note in auto mode.
func (e *effortPlan) recall(in *core.Input, promptField, task string, auto bool) string {
	recallBlock, _ := memhook.AgentRecall(in.SessionID, in.CWD, task)
	if recallBlock == "" {
		return ""
	}
	if auto {
		return "memory-recall for this dispatch (paste into the brief if relevant):" + recallBlock
	}
	e.updated[promptField] = task + recallBlock
	e.changed = true
	return ""
}

// profile applies a brief's Profile: line; a non-nil result denies the dispatch.
func (e *effortPlan) profile(promptField, task string) *core.Result {
	profileNote, deny := applyProfile(e.updated, promptField, task)
	if deny != nil {
		return deny
	}
	if profileNote != "" {
		e.note, e.changed = strings.TrimSpace(e.note+" "+profileNote), true
	}
	return nil
}

// label puts the model the dispatch runs on first in its description: a rewrite normally, a one-time deny in auto
// mode, where a rewritten tool input is rejected by the classifier.
func (e *effortPlan) label(in *core.Input, task string, auto bool) *core.Result {
	session := func() string { u, _ := transcript.LastUsage(in.TranscriptPath); return u.Model }
	if !auto {
		e.changed = applyLabel(e.updated, task, session) || e.changed
		return nil
	}
	probe := maps.Clone(e.updated)
	if !applyLabel(probe, task, session) {
		return nil
	}
	return &core.Result{Decision: core.Block, Reason: fmt.Sprintf("agent-effort: the description must start with the model the "+
		"dispatch runs on. Resend this dispatch with description %q (or print the prefix with `claude-code agents label <type>`).", probe["description"])}
}

// discovery appends the bring-your-own-key hint; outside auto mode it counts as a change.
func (e *effortPlan) discovery(in *core.Input, task string, auto bool) {
	if hint := discoveryHint(in.Nested("tool_input", "subagent_type"), task); hint != "" {
		e.note, e.changed = strings.TrimSpace(e.note+"\n"+hint), e.changed || !auto
	}
}

func (e *effortPlan) result() core.Result {
	switch {
	case e.changed:
		return core.Result{UpdatedInput: e.updated, Context: e.note, SystemMessage: e.systemMessage}
	case e.note != "":
		return core.Result{Context: e.note, SystemMessage: e.systemMessage}
	}
	return core.Result{}
}

// applyLabel prefixes the description with the agents doing the work, minus the leaf's own type.
func applyLabel(updated map[string]any, task string, session func() string) bool {
	st, _ := updated["subagent_type"].(string)
	orig, _ := updated["description"].(string)
	desc := orig
	if rest, ok := strings.CutPrefix(desc, st+":"); ok { // a hand-typed own-type prefix ("Plan: …") repeats the row
		desc = strings.TrimSpace(rest)
	}
	label, desc := leafLabel(updated, st, task, desc, session)
	out, ok := labelled(label, desc)
	if !ok || out == orig {
		return false
	}
	updated["description"] = out
	return true
}

// leafLabel picks the prefix: a harness leaf's agents resolved to their model, else the leaf's own model.
func leafLabel(updated map[string]any, st, task, desc string, session func() string) (string, string) {
	label, compare := harnessLabel(st, task), modeCompareRe.MatchString(task)
	if label != "" && !compare {
		desc = cutLabel(desc, label)                     // a hand-typed or earlier unsuffixed label
		if m := labelModel(updated, st, task); m != "" { // the model id alone says where the work runs
			return m, cutLabel(desc, m)
		}
		return label, desc
	}
	if label == "" && !compare && strings.TrimSpace(desc) != "" { // every other leaf leads with its own model
		m := ownModel(updated, st, session)
		if routedOwn(st, task) { // an external agent or a Profile: runs on its own cell, not the dispatch alias
			m = labelModel(updated, st, task)
		}
		if m != "" {
			return m, cutLabel(desc, m)
		}
	}
	return label, desc
}

// harnessLabel names the agents a harness leaf requests; other types carry no label.
func harnessLabel(st, task string) string {
	if !labelAgents[st] {
		return ""
	}
	return agentLabel(st, task)
}

func cutLabel(desc, label string) string {
	if rest, ok := strings.CutPrefix(desc, label+":"); ok {
		return strings.TrimSpace(rest)
	}
	return desc
}

// labelled joins label and desc; ok is false when desc already is the bare label.
func labelled(label, desc string) (string, bool) {
	switch {
	case label == "":
		return lowerFirst(desc), true
	case strings.HasPrefix(desc, label+":"):
		return label + ": " + lowerFirst(strings.TrimPrefix(desc[len(label)+1:], " ")), true
	case desc == label:
		return "", false
	case strings.TrimSpace(desc) == "":
		return label, true
	}
	return label + ": " + lowerFirst(desc), true
}

// ownModel is the dispatch's model, else the agent file's, else (also for `inherit`) the session's; "" labels nothing.
func ownModel(updated map[string]any, st string, session func() string) string {
	m, _ := updated["model"].(string)
	if m == "" || m == "inherit" {
		m = rosterModel(st)
	}
	if m == "" || m == "inherit" {
		return session()
	}
	return m
}

// routedOwn is true when the leaf runs on an external agent by its own type or on a Profile: the brief applies.
func routedOwn(st, task string) bool {
	return externalTypes[st] || profileApplies(st, task)
}

// profileApplies is true when the brief's Profile: line reaches a leaf of type st (read-only and in-context leaves ignore it).
func profileApplies(st, task string) bool {
	return briefProfile(task) != "" && !readOnlyTypes[st] && !inContextBrief(task)
}

// modeCompareRe matches a `Mode: compare` line in a brief.
var modeCompareRe = regexp.MustCompile(`(?im)^\s*Mode:\s*compare\b`)

// agentLabel is the "a → b" (relay) or "a, b" (`Mode: compare`) label the `Agent:` line earns, or "" when there is none.
func agentLabel(st, task string) string {
	m := agentLineRe.FindStringSubmatch(task)
	if m == nil && briefProfile(task) == "" {
		return ""
	}
	var names []string
	for _, w := range briefAgents(task) {
		if w != st {
			names = append(names, w)
		}
	}
	if modeCompareRe.MatchString(task) { // a compare fans out in parallel: names, not a chain
		return profileSuffix(strings.Join(names, ", "), task)
	}
	return profileSuffix(strings.Join(names, " → "), task)
}

// lowerFirst lowercases desc's first letter unless its first word looks like an acronym or identifier.
func lowerFirst(desc string) string {
	word := strings.TrimRight(strings.Fields(desc + " x")[0], ".,:;!?")
	if strings.ContainsAny(word, "._/`0123456789") {
		return desc
	}
	r := []rune(desc)
	i := 0
	for i < len(r) && unicode.IsSpace(r[i]) {
		i++
	}
	if i >= len(r) || !unicode.IsUpper(r[i]) || (i+1 < len(r) && unicode.IsUpper(r[i+1])) {
		return desc
	}
	r[i] = unicode.ToLower(r[i])
	return string(r)
}

// pinModel fills updated["model"] from the classifier when the dispatch left it unset,
// and returns the model note and visible reason ("" when nothing was pinned).
func pinModel(in *core.Input, updated map[string]any, task string) (note, systemMessage string) {
	// An explicit model is a deliberate choice by whoever dispatched — never second-guess it.
	// `inherit` is not one of those: it is the default spelling for "unset", and honoring it ran
	// every unpinned spawn on the parent session's model. The roster's own pin outranks every
	// decision too: a model-pinned agent already carries its tier.
	if explicitModel(in) || task == "" || agentPinsModel(in.Nested("tool_input", "subagent_type")) {
		return "", ""
	}
	tier, signal := router.EffortTierSignal(task)
	model := models.AliasModel(tier)
	if model == "" { // a tier with no row: never emit an empty pin
		return "", ""
	}
	updated["model"] = model
	return fmt.Sprintf("agent-effort: dispatch model unset and the task classifies %s — pinned model=%s"+
			" (set model explicitly to override).", tier, model),
		fmt.Sprintf("agent-effort: %s → %s (%s)", tier, model, signal)
}

// explicitModel is a dispatch-set model other than `inherit` (the default spelling for unset).
func explicitModel(in *core.Input) bool {
	m := strings.TrimSpace(in.Nested("tool_input", "model"))
	return m != "" && !strings.EqualFold(m, "inherit")
}

// agentFile is the roster file of an agent type; ok is false for an empty or path-shaped name.
func agentFile(name string) (string, bool) {
	if name == "" || filepath.Base(name) != name {
		return "", false
	}
	return filepath.Join(paths.ConfigDir(), "agents", name+".md"), true
}

// rosterModel is the named harness agent's frontmatter model ("" for a built-in or a path-shaped name).
func rosterModel(name string) string {
	file, ok := agentFile(name)
	if !ok {
		return ""
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return ""
	}
	fm, _ := textutil.ParseFrontmatter(string(raw))
	model, _ := fm["model"].(string)
	return model
}

// agentPinsModel reports whether the named subagent's frontmatter sets any model, `inherit` included (it
// asks for the session model). No roster file = a built-in, the classifier applies; a path-shaped name is refused.
func agentPinsModel(name string) bool {
	if name == "" {
		return false // no subagent_type = a fresh general-purpose agent, nothing to defer to
	}
	file, ok := agentFile(name)
	if !ok {
		return true // path-shaped input is not a roster name; refuse to touch the dispatch
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return false // not a harness agent (built-in like Explore/Plan) — classifier may apply
	}
	fm, _ := textutil.ParseFrontmatter(string(raw))
	model, _ := fm["model"].(string)
	return model != ""
}

// labelModel is the model id a leaf is planned to run: the Claude pin in-context, else the profile's or route table's
// model for its last agent at the leaf's tier; "" when unknown (the label falls back to the agent name).
func labelModel(updated map[string]any, st, task string) string {
	alias, _ := updated["model"].(string)
	if alias == "" {
		alias = rosterModel(st)
	}
	names, p := briefAgents(task), ""
	if profileApplies(st, task) {
		p = briefProfile(task)
	}
	if agentLineRe.FindString(task) == "" && labelAgents[st] {
		names = []string{st} // an agent-named leaf with no Agent: line runs on its own agent
	}
	if len(names) == 0 || names[len(names)-1] == "claude" && p == "" {
		return alias
	}
	if !slices.Contains(models.AliasNames, alias) {
		alias = models.Sonnet
	}
	if p != "" {
		return labelProfileModel(p, alias)
	}
	return labelCell(names[len(names)-1], alias)
}

// Label lookups are seams: tests stub them so a dispatch label never reads the live vault or route table.
var (
	labelProfileModel = profileModelFor
	labelCell         = cellModelFor
)

func profileModelFor(profile, alias string) string {
	reg, err := providers.Load()
	if err != nil || reg.Providers[profile] == nil {
		return ""
	}
	return models.Strip1M(reg.Providers[profile].Models[alias])
}

func cellModelFor(agent, alias string) string {
	t, err := router.Load()
	if err != nil {
		return ""
	}
	c, _ := t.Cell(agent, alias)
	pins, err := agentpkg.AgentPins()
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-effort label: %v\n", err)
		return ""
	}
	pin := pins[agent]
	if pin.Profile == "" && pin.Model == "" {
		return c.Model
	}
	reg, err := providers.Load()
	if err != nil {
		return c.Model
	}
	m, _ := router.PinnedModel(agent, alias, c, reg.Providers[pin.Profile], pin.Model)
	return m
}

// discoveryPoolSize is the curated Discovery pool's model count (a var so tests can pin it).
var discoveryPoolSize = func() int { return len(router.DiscoveryPool()) }

// discoveryHint is the one-line `--model discovery` reminder for a brief whose Agent: line names copilot or
// opencode; "" when neither applies or the brief already passes --model.
func discoveryHint(st, task string) string {
	if strings.Contains(task, "--model") {
		return ""
	}
	hit := st == "copilot" || st == "opencode"
	for _, w := range agentNameRe.FindAllString(agentLabel(st, task), -1) {
		hit = hit || w == "copilot" || w == "opencode"
	}
	if !hit {
		return ""
	}
	return fmt.Sprintf("discovery: run copilot/opencode with --model discovery — "+
		"the router picks from the curated Databricks pool (%d models)", discoveryPoolSize())
}
