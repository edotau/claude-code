// `dispatch-effort`: PreToolUse on a spawn/Workflow — type deny, model pin, recall, Profile:/Route:, label; fails OPEN.
package agents

import (
	"cmp"
	"context"
	"errors"
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
	direct := directAsk(task)
	auto := in.Nested("permission_mode") == "auto" || direct
	session := func() string { u, _ := transcript.LastUsage(in.TranscriptPath); return u.Model }
	e := &effortPlan{updated: updated, run: newDispatchRun(session)}
	recallNote := e.recall(in, promptField, task, auto)
	if !direct {
		e.note, e.systemMessage = pinModel(e.run, in, updated, task)
		e.changed = e.changed || e.note != ""
	}
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

// directAskRe matches a brief's opening ask command, with or without the global --remote <host> in front.
var directAskRe = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^Run: claude-code (--remote \S+ )?agents ask `)
})

// runModelRe captures --model on a brief's opening ask command.
var runModelRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`\s--model[ =](\S+)`) })

// runLineModel is the --model a directAsk brief's first line names, or "".
func runLineModel(task string) string {
	if !directAsk(task) {
		return ""
	}
	line, _, _ := strings.Cut(task, "\n")
	if m := runModelRe().FindStringSubmatch(line); m != nil {
		return m[1]
	}
	return ""
}

// directAsk is a brief that opens with its own ask command (the /<agent> and /coworker commands): never rewritten.
func directAsk(task string) bool {
	return directAskRe().MatchString(task) && briefProfile(task) == "" && briefRoute(task) == ""
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
		fmt.Fprintf(os.Stderr, "dispatch-effort: no agent definitions read from %s (%v); allowing the roster\n", dir, err)
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
	return &core.Result{Decision: core.Block, Reason: fmt.Sprintf("dispatch-effort: subagent_type %q is not on the roster (%s); %s",
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
	switch agent {
	case "":
		return "name one roster agent, or use the /<agent> command"
	case agentid.Claude: // no /claude command: the name could collide
		return `use "claude", or the /coworker --agent claude command`
	}
	return fmt.Sprintf("use %q, or the /%s command", agent, agent)
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
	return &core.Result{Decision: core.Block, Reason: fmt.Sprintf("dispatch-effort: the description must start with the model the "+
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
		if !ok || head == "" || (head != st && (label == "" || !labelShaped(head, label))) {
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
	updated["model"] = model
	return fmt.Sprintf("dispatch-effort: dispatch model unset and the task classifies %s — pinned model=%s"+
			" (set model explicitly to override).", tier, model),
		fmt.Sprintf("dispatch-effort: %s → %s (%s)", tier, model, signal)
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
	if m := runLineModel(task); m != "" {
		return m // the brief's own ask command names the model: the label says what runs
	}
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
		fmt.Fprintf(os.Stderr, "dispatch-effort label: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "dispatch-effort label: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "dispatch-effort label: %v\n", err)
		return ""
	}
	c, _ := t.Cell(agent, alias)
	if pin.Profile == "" && pin.Model == "" {
		return c.Model
	}
	reg, err := run.registry()
	if err != nil {
		fmt.Fprintf(os.Stderr, "dispatch-effort label: %v\n", err)
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
	for _, w := range briefAgents(task) {
		hit = hit || w == "copilot" || w == "opencode"
	}
	if !hit {
		return ""
	}
	return fmt.Sprintf("discovery: run copilot/opencode with --model discovery — "+
		"the router picks from the curated Databricks pool (%d models)", discoveryPoolSize())
}

// `Profile:` brief line: a dispatch for another profile keeps its type and runs `ask --profile <p>`.
var profileLineRe = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?m)^[ \t]*Profile:[ \t]*([A-Za-z0-9._-]+)`)
})

// profileNames lists the registered profile names; ok is false when the registry cannot load (fail open).
var profileNames = func(run *dispatchRun) ([]string, bool) {
	reg, err := run.registry()
	if err != nil {
		return nil, false
	}
	return reg.Names(), true
}

// briefProfile is the profile the brief's `Profile:` line names, or "".
func briefProfile(task string) string {
	if m := profileLineRe().FindStringSubmatch(task); m != nil {
		return strings.TrimRight(m[1], ".") // "Profile: dev." ends a sentence, not the name
	}
	return ""
}

// briefAgents are the agents the `Agent:` line names, claude when the line is absent; nil for in-context.
func briefAgents(task string) []string {
	m := agentLineRe().FindStringSubmatch(task)
	if m == nil {
		return []string{"claude"}
	}
	line := strings.ToLower(m[1])
	if strings.Contains(line, "in-context") {
		return nil
	}
	var names []string
	for _, w := range agentNameRe().FindAllString(line, -1) {
		if labelAgent(w) && !slices.Contains(names, w) {
			names = append(names, w)
		}
	}
	return names
}

// readOnlyTypes are subagent types without write tools; a Profile: line does not apply to them.
var readOnlyTypes = map[string]bool{"Plan": true, "Explore": true, "gemini": true}

// inContextBrief is true when the brief's Agent: line says in-context.
func inContextBrief(task string) bool {
	m := agentLineRe().FindStringSubmatch(task)
	return m != nil && strings.Contains(strings.ToLower(m[1]), "in-context")
}

// applyProfile prepends the exact ask command to a Profile: dispatch. deny is a Block
// for an unknown profile; note is "" when the brief names no profile or the dispatch is already routed.
func applyProfile(run *dispatchRun, updated map[string]any, promptField, task string) (note string, deny *core.Result) {
	p := briefProfile(task)
	if p == "" {
		return "", nil
	}
	if arm := router.ArmProfile(); arm != "" && p != arm {
		return "", &core.Result{Decision: core.Block, Reason: "dispatch-effort: Profile: " + p + " conflicts with databricks:" + arm}
	}
	if names, ok := profileNames(run); ok && !slices.Contains(names, p) {
		return "", &core.Result{Decision: core.Block, Reason: fmt.Sprintf(
			"dispatch-effort: unknown Profile: %q — valid profiles: %s.", p, strings.Join(names, ", "))}
	}
	st, _ := updated["subagent_type"].(string)
	if readOnlyTypes[st] {
		return "dispatch-effort: Profile: " + p + " ignored — " + st + " is read-only and runs on the session route.", nil
	}
	if inContextBrief(task) {
		return "dispatch-effort: Profile: " + p + " ignored for an in-context leaf.", nil
	}
	return routeProfile(updated, promptField, task, p), nil
}

// routeProfile prepends the ask command to the prompt, once.
func routeProfile(updated map[string]any, promptField, task, p string) (note string) {
	agents := briefAgents(task)
	if st, _ := updated["subagent_type"].(string); externalTypes[st] && !agentLineRe().MatchString(task) {
		agents = []string{st} // with no Agent: line an external agent type runs as itself, as under Route:
	}
	if len(agents) == 0 {
		agents = []string{"claude"}
	}
	var cmds []string
	for _, a := range agents {
		cmds = append(cmds, fmt.Sprintf("claude-code agents ask --agent %s --profile %s", a, p))
	}
	prompt, _ := updated[promptField].(string)
	if !strings.Contains(prompt, "--profile "+p) {
		updated[promptField] = "Run: " + strings.Join(cmds, " ; ") + " (profile " + p + "; ask resolves host and auth)\n\n" + prompt
		note = "dispatch-effort: Profile: " + p + " — leaf must run `" + strings.Join(cmds, "`, `") + "`."
	}
	return note
}

var routeLineRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?m)^[ \t]*Route:[ \t]*(\S+)`) })

// briefRoute is the `Route:` line's value, or "".
func briefRoute(task string) string {
	if m := routeLineRe().FindStringSubmatch(task); m != nil {
		return strings.TrimRight(m[1], ".")
	}
	return ""
}

// routeApplies is true when a brief's Route: reaches a leaf of type st: Explore, Plan and in-context leaves ignore it.
func routeApplies(st, task string) bool {
	return briefRoute(task) != "" && (!readOnlyTypes[st] || externalTypes[st]) && !inContextBrief(task)
}

// routeWorkspace is the workspace a brief's Route: line names (bare: the armed route's, else the session's profile).
func routeWorkspace(task string) (string, error) {
	mode, p, err := router.ParseRoute(briefRoute(task))
	switch {
	case err != nil:
		return "", err
	case mode != router.RouteDatabricks:
		return "", fmt.Errorf("brief Route: %s: only databricks[:<profile>] is a brief route", mode)
	case p == "":
		p = cmp.Or(router.ArmProfile(), providers.SessionProfile())
	}
	if p == "" {
		return "", errors.New("brief Route: databricks needs a workspace: databricks:<profile>")
	}
	return p, nil
}

// servingRow refuses a profile route databricks:<p> cannot run on; tests stub it.
var servingRow = providers.ServingRow

// applyRoute prepends `agents ask --route databricks:<p>` to a Route: dispatch; deny blocks a bad, unknown or
// conflicting route (another Profile:, or leaving the session's armed route). Inside the same route it is a no-op.
func applyRoute(run *dispatchRun, updated map[string]any, promptField, task string) (note string, deny *core.Result) {
	st, _ := updated["subagent_type"].(string)
	if !routeApplies(st, task) {
		return "", nil
	}
	p, err := routeWorkspace(task)
	block := func(msg string) (string, *core.Result) {
		return "", &core.Result{Decision: core.Block, Reason: "dispatch-effort: " + msg}
	}
	switch arm := router.ArmProfile(); {
	case err != nil:
		return block(err.Error())
	case arm != "" && p != arm:
		return block("Route: databricks:" + p + " conflicts with databricks:" + arm)
	case briefProfile(task) != "" && briefProfile(task) != p:
		return block("Route: databricks:" + p + " conflicts with Profile: " + briefProfile(task))
	case arm == p:
		return "", nil
	}
	if names, ok := profileNames(run); ok && !slices.Contains(names, p) {
		return block(fmt.Sprintf("--route databricks:%s: no such profile", p))
	}
	if err := servingRow(p); err != nil {
		return block(err.Error())
	}
	cmd := fmt.Sprintf("claude-code agents ask --agent %s --route databricks:%s", routeAgent(st, task), p)
	if prompt, _ := updated[promptField].(string); !strings.Contains(prompt, "--route databricks:"+p) {
		updated[promptField] = "Run: " + cmd + " (route databricks:" + p + "; ask resolves host and auth)\n\n" + prompt
		note = "dispatch-effort: Route: databricks:" + p + " — leaf must run `" + cmd + "`."
	}
	return note, nil
}

// routeAgent is the agent a Route: dispatch asks for: the work agent, else claude.
func routeAgent(st, task string) string {
	return cmp.Or(workAgent(st, task), agentid.Claude)
}
