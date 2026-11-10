// `agent-effort` — PreToolUse on a subagent spawn (Claude Code's Agent/Task tool) or a Workflow: fills an UNSET
// dispatch model from the local prompt classifier (low->haiku, medium/high->sonnet). Never overrides
// an explicit model/pin; fails OPEN on any error.
package agents

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"unicode"

	agentpkg "github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig/project"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hooks/quality"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/transcript"
)

// agentLineRe matches a brief's `Agent:` line; agentNameRe the words in it.
var (
	agentLineRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?m)^[ \t]*Agent:[ \t]*(.+)$`) })
	agentNameRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`[a-z]+`) })
)

// dispatchRun memoizes one hook run's agent-file and provider-registry reads; session is the session's model id.
type dispatchRun struct {
	session  func() string
	roster   map[string]string
	registry func() (*providers.Registry, error)
}

// Per-run read seams: tests count them; production reads the roster file and the provider registry.
var (
	readAgentFile = os.ReadFile
	loadRegistry  = providers.Load
)

func newDispatchRun(session func() string) *dispatchRun {
	return &dispatchRun{session: session, roster: map[string]string{}, registry: sync.OnceValues(loadRegistry)}
}

// rosterModel is rosterModel(name), read once per run.
func (r *dispatchRun) rosterModel(name string) string {
	m, ok := r.roster[name]
	if !ok {
		m = rosterModel(name)
		r.roster[name] = m
	}
	return m
}

// agentEffortResult is the pin step as a Result: a spawn gets one UpdatedInput (model pin, recall block, label);
// a Workflow gets the former workflow-lint rewrite or a Block.
func agentEffortResult(in *core.Input) core.Result {
	if in.ToolName == "Workflow" {
		return quality.WorkflowResult(in)
	}
	if !core.SpawnTools[in.ToolName] {
		return core.Result{}
	}
	if deny := typeBlock(in.Nested("tool_input", "subagent_type"), in.Nested("tool_input", "prompt")); deny != nil {
		return *deny
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
	session := func() string { u, _ := transcript.LastUsage(in.TranscriptPath); return u.Model }
	e := &effortPlan{updated: updated, run: newDispatchRun(session)}
	recallNote := e.recall(in, promptField, task, auto)
	e.note, e.systemMessage = pinModel(e.run, in, updated, task)
	e.changed = e.changed || e.note != ""
	if deny := e.profile(promptField, task); deny != nil {
		return *deny
	}
	if deny := e.route(promptField, task); deny != nil {
		return *deny
	}
	if deny := e.label(task, auto); deny != nil {
		return *deny
	}
	e.discovery(in, task, auto)
	e.note = strings.TrimSpace(e.note + "\n" + recallNote)
	return e.result()
}

// builtinTypes are the harness built-in Claude-run leaf types with no agents/<name>.md that a dispatch may use.
var builtinTypes = []string{"Explore", "general-purpose"}

// listAgents loads the agent definitions; a seam so tests count or fail the read.
var listAgents = project.DiscoverAgents

// dispatchTypes is every allowed subagent_type, sorted: each agents/<name>.md plus builtinTypes; a dir that is
// unreadable or holds no agent falls back to the roster with one notice, so it never blocks every dispatch.
func dispatchTypes() []string {
	dir := filepath.Join(paths.ConfigDir(), "agents")
	srcs, err := listAgents(dir)
	names := slices.Clone(builtinTypes)
	for _, s := range srcs {
		names = append(names, s.Name())
	}
	if err != nil || len(srcs) == 0 {
		fmt.Fprintf(os.Stderr, "agent-effort: no agent definitions read from %s (%v); allowing the roster\n", dir, err)
		names = append(slices.Clone(builtinTypes), agentid.All()...)
	}
	slices.Sort(names)
	return names
}

// typeBlock denies a spawn whose subagent_type is not allowed, naming the type to use instead; "" runs general-purpose.
func typeBlock(st, task string) *core.Result {
	if st == "" {
		return nil
	}
	allowed := dispatchTypes()
	if slices.Contains(allowed, st) {
		return nil
	}
	return &core.Result{Decision: core.Block, Reason: fmt.Sprintf("agent-effort: subagent_type %q is not on the roster (%s); %s",
		st, strings.Join(allowed, ", "), typeSuggestion(st, task))}
}

// typeSuggestion is the redirect: a coworker type to the roster agent it wraps, anything else to general-purpose.
func typeSuggestion(st, task string) string {
	agent, isCoworker := strings.CutPrefix(st, "coworker:")
	switch {
	case st == "Plan":
		return `use "claude" and state read-only in the brief`
	case st == "coworker" || isCoworker && agent == "databricks":
		agent = briefExternal(task)
	case !isCoworker:
		return `use "general-purpose"` // every tool, WebFetch included: covers web-fetch, claude-code-guide, plugin agents
	}
	if agent == "" {
		return "name one roster agent, or use the /coworker:<agent> command"
	}
	return fmt.Sprintf("use %q, or the /coworker:%s command", agent, agent)
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
	run           *dispatchRun
	updated       map[string]any
	note          string
	systemMessage string
	changed       bool
}

// recall appends the memory-recall block to the prompt, or returns it as a note in auto mode.
func (e *effortPlan) recall(in *core.Input, promptField, task string, auto bool) string {
	recallBlock, _ := memory.AgentRecall(in.SessionID, in.CWD, task)
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
	profileNote, deny := applyProfile(e.run, e.updated, promptField, task)
	if deny != nil {
		return deny
	}
	if profileNote != "" {
		e.note, e.changed = strings.TrimSpace(e.note+" "+profileNote), true
	}
	return nil
}

// route applies a brief's Route: line; a non-nil result denies the dispatch.
func (e *effortPlan) route(promptField, task string) *core.Result {
	note, deny := applyRoute(e.run, e.updated, promptField, task)
	if note != "" {
		e.note, e.changed = strings.TrimSpace(e.note+" "+note), true
	}
	return deny
}

// label puts the model the dispatch runs on first in its description: a rewrite normally, a one-time deny in auto
// mode, where a rewritten tool input is rejected by the classifier.
func (e *effortPlan) label(task string, auto bool) *core.Result {
	if !auto {
		changed, _ := applyLabel(e.run, e.updated, task)
		e.changed = changed || e.changed
		return nil
	}
	probe := maps.Clone(e.updated)
	if _, canonical := applyLabel(e.run, probe, task); canonical { // a right prefix passes; case alone never blocks
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

// applyLabel rewrites the description to `<model>: <task>`; canonical is false when its prefix is missing or wrong.
func applyLabel(run *dispatchRun, updated map[string]any, task string) (changed, canonical bool) {
	st, _ := updated["subagent_type"].(string)
	orig, _ := updated["description"].(string)
	if strings.TrimSpace(orig) == "" {
		return false, true
	}
	label := labelModel(run, updated, st, task)
	rest := stripLabels(orig, st, label)
	out := lowerFirst(rest)
	if label != "" {
		out = label + ": " + out
	}
	if out == orig {
		return false, true
	}
	updated["description"] = out
	return true, label == "" || orig == label+": "+rest
}

// stripLabels cuts every leading label-shaped `<head>:` so a doubled or wrong prefix is replaced, never stacked.
func stripLabels(desc, st, label string) string {
	for {
		head, rest, ok := strings.Cut(desc, ":")
		head = strings.TrimSpace(head)
		if !ok || head == "" || !(head == st || label != "" && labelShaped(head, label)) {
			return strings.TrimSpace(desc)
		}
		desc = rest
	}
}

// modelIDRe matches a model id: no spaces, a letter, a digit and a `-` or `/` ("gpt-6.1-sol", "opencode-go/kimi-k3").
var modelIDRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[A-Za-z][\w.]*[-/][\w./-]*$`) })

// labelShaped is true for a description head that is a dispatch label rather than task text.
func labelShaped(head, label string) bool {
	switch {
	case head == label || slices.Contains(models.AliasNames, head) || agentid.Known(head):
		return true
	}
	return modelIDRe().MatchString(head) && strings.ContainsAny(head, "0123456789")
}

// ownModel is the dispatch's model, else the agent file's, else (also for `inherit`) the session's; "" labels nothing.
func ownModel(run *dispatchRun, updated map[string]any, st string) string {
	m, _ := updated["model"].(string)
	if m == "" || m == "inherit" {
		m = run.rosterModel(st)
	}
	if m == "" || m == "inherit" {
		return run.session()
	}
	return m
}

// workAgent is the external agent doing the work ("" = Claude): the type, or for claude its Agent: line's one agent.
func workAgent(st, task string) string {
	if externalTypes[st] {
		return st
	}
	if st != agentid.Claude {
		return ""
	}
	return briefExternal(task)
}

// briefExternal is the one external agent the brief's Agent: line names; "" for none or several.
func briefExternal(task string) string {
	var named []string
	for _, a := range briefAgents(task) {
		if externalTypes[a] {
			named = append(named, a)
		}
	}
	if len(named) != 1 {
		return ""
	}
	return named[0]
}

// profileApplies is true when the brief's Profile: line reaches a leaf of type st (read-only and in-context leaves ignore it).
func profileApplies(st, task string) bool {
	return briefProfile(task) != "" && !readOnlyTypes[st] && !inContextBrief(task)
}

// modeCompareRe matches a `Mode: compare` line in a brief.
var modeCompareRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?im)^\s*Mode:\s*compare\b`) })

// agentLabel is the "a → b" (relay) or "a, b" (`Mode: compare`) label the `Agent:` line earns, or "" when there is none.
func agentLabel(st, task string) string {
	m := agentLineRe().FindStringSubmatch(task)
	if m == nil && briefProfile(task) == "" {
		return ""
	}
	var names []string
	for _, w := range briefAgents(task) {
		if w != st {
			names = append(names, w)
		}
	}
	if modeCompareRe().MatchString(task) { // a compare fans out in parallel: names, not a chain
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
func pinModel(run *dispatchRun, in *core.Input, updated map[string]any, task string) (note, systemMessage string) {
	// An explicit model is a deliberate choice by whoever dispatched — never second-guess it.
	// `inherit` is not one of those: it is the default spelling for "unset", and honoring it ran
	// every unpinned spawn on the parent session's model. The roster's own pin outranks every
	// decision too: a model-pinned agent already carries its tier.
	if explicitModel(in) || task == "" || agentPinsModel(run, in.Nested("tool_input", "subagent_type")) {
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

// agentFile is the agents/<name>.md file of an agent type; ok is false for an empty or path-shaped name.
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
	raw, err := readAgentFile(file)
	if err != nil {
		return ""
	}
	fm, _ := textutil.ParseFrontmatter(string(raw))
	model, _ := fm["model"].(string)
	return model
}

// agentPinsModel reports whether the named subagent's frontmatter sets any model, `inherit` included (it
// asks for the session model). No roster file = a built-in, the classifier applies; a path-shaped name is refused.
func agentPinsModel(run *dispatchRun, name string) bool {
	if name == "" {
		return false // no subagent_type = a fresh general-purpose agent, nothing to defer to
	}
	if _, ok := agentFile(name); !ok {
		return true // path-shaped input is not a roster name; refuse to touch the dispatch
	}
	return run.rosterModel(name) != "" // no roster file = a built-in (Explore/Plan), the classifier may apply
}

// labelModel is the one precedence: Route: pick, Profile: model, workAgent's no-run pick, else the leaf's Claude model.
func labelModel(run *dispatchRun, updated map[string]any, st, task string) string {
	agent, profile, route := workAgent(st, task), profileApplies(st, task), routeApplies(st, task)
	if agent == "" && !profile && !route {
		return ownModel(run, updated, st)
	}
	tier, _ := updated["model"].(string)
	if tier == "" || tier == "inherit" {
		tier = run.rosterModel(st)
	}
	if !slices.Contains(models.AliasNames, tier) {
		tier = models.Sonnet
	}
	if p, err := routeWorkspace(task); route && err == nil {
		return routePick(run, p, routeAgent(st, task), tier)
	}
	if profile {
		return labelProfileModel(run, briefProfile(task), tier)
	}
	return labelCell(run, agent, tier)
}

// routePick is labelCell inside route databricks:<p>: the env `--route` exports (ask.ApplyRoute), restored after.
func routePick(run *dispatchRun, p, agent, tier string) string {
	for k, v := range map[string]string{router.EnvRoute: router.RouteDatabricks + ":" + p, providers.EnvSessionProfile: p} {
		old, had := os.LookupEnv(k)
		_ = os.Setenv(k, v)
		defer restoreEnv(k, old, had)
	}
	return labelCell(run, agent, tier)
}

func restoreEnv(k, old string, had bool) {
	if had {
		_ = os.Setenv(k, old)
		return
	}
	_ = os.Unsetenv(k)
}

// Label lookups are seams: tests stub them so a dispatch label never reads the live vault or route table.
var (
	labelProfileModel = profileModelFor
	labelCell         = cellModelFor
	labelPick         = previewModel
)

func profileModelFor(run *dispatchRun, profile, alias string) string {
	reg, err := run.registry()
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-effort label: %v\n", err)
		return ""
	}
	if reg.Providers[profile] == nil {
		return ""
	}
	return models.Strip1M(reg.Providers[profile].Models[alias])
}

// cellModelFor is the model a leaf runs: the first candidate of the no-run route preview (what `gateway` picks), else
// the table prediction when the preview finds nothing runnable.
func cellModelFor(run *dispatchRun, agent, alias string) string {
	pins, err := agentpkg.AgentPins()
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-effort label: %v\n", err)
		return ""
	}
	if slices.ContainsFunc(slices.Collect(maps.Values(pins)), func(s router.PinSpec) bool { return s.Model == router.DiscoveryModel }) {
		return "" // a discovery pick advances the rotation in quota.json; a label never writes state
	}
	if m := labelPick(agent, alias); m != "" {
		return m
	}
	return tableCellModel(run, agent, alias, pins[agent])
}

// previewModel is the preview's first candidate model without its `<profile>:` prefix; "" when nothing runs.
func previewModel(agent, alias string) string {
	_, res, err := agentpkg.Resolve(context.Background(), agentpkg.RouteReq{Agent: agent, Model: alias, Fallback: router.FallbackAll, Preview: true})
	if err != nil || len(res.Candidates) == 0 {
		return ""
	}
	c := res.Candidates[0]
	return strings.TrimPrefix(c.Model, c.Profile+":")
}

// tableCellModel predicts the model from the route table cell and the agent's pin, without running the pick.
func tableCellModel(run *dispatchRun, agent, alias string, pin router.PinSpec) string {
	t, err := router.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-effort label: %v\n", err)
		return ""
	}
	c, _ := t.Cell(agent, alias)
	if pin.Profile == "" && pin.Model == "" {
		return c.Model
	}
	reg, err := run.registry()
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-effort label: %v\n", err)
		return c.Model
	}
	m, _ := router.PinnedModel(agent, alias, c, reg.Providers[pin.Profile], pin.Model)
	return m
}

// discoveryPoolSize is the curated Discovery pool's model count (a var so tests can pin it).
var discoveryPoolSize = func() int { return len(router.DiscoveryPool()) }

// discoveryHint is the `--model discovery` reminder for a copilot/opencode spawn or Agent: line; "" with --model.
func discoveryHint(st, task string) string {
	if strings.Contains(task, "--model") {
		return ""
	}
	hit := st == "copilot" || st == "opencode"
	for _, w := range agentNameRe().FindAllString(agentLabel(st, task), -1) {
		hit = hit || w == "copilot" || w == "opencode"
	}
	if !hit {
		return ""
	}
	return fmt.Sprintf("discovery: run copilot/opencode with --model discovery — "+
		"the router picks from the curated Databricks pool (%d models)", discoveryPoolSize())
}
