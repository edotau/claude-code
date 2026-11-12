// `dispatch-effort`: PreToolUse on spawn/Workflow: type deny, model pin, recall, Profile:/Coworker:, label; fails OPEN.
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
	"github.com/edotau/claude-code/internal/agentconfig"
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

// agentLineRe matches a brief's `Agent:` line (read from briefHeader only); agentNameRe the words in it.
var (
	agentLineRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?m)^[ \t]*Agent:[ \t]*(.+)$`) })
	agentNameRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`[a-z]+`) })
)

// dispatchRun memoizes one hook run's agent-file and provider-registry reads; session is the session's model id.
type dispatchRun struct {
	session  func() string
	roster   map[string]string
	registry func() (*providers.Registry, error)
	line     *askLine // the brief's opening ask command, parsed (and its overlay loaded) once
	lineTask string
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
	if deny := e.rewrite(promptField, task, auto); deny != nil {
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

// opensWithAsk is a task whose first line, past leading blank space as briefHeader reads it, is an ask command.
func opensWithAsk(task string) bool { return directAskRe().MatchString(strings.TrimSpace(task)) }

// askLine is a brief's opening ask command: its --agent and --model, and its --overlay document when one loads.
type askLine struct {
	agent, model string
	cfg          agentconfig.Overlay
}

// opening is task's opening ask command, parsed once per run; zero for a brief that does not open with one.
func (r *dispatchRun) opening(task string) askLine {
	if r.line == nil || r.lineTask != task {
		l := parseAskLine(task)
		r.line, r.lineTask = &l, task
	}
	return *r.line
}

// parseAskLine reads the flags a shell would see on the opening ask command; text inside a quoted task is no flag.
func parseAskLine(task string) askLine {
	if !opensWithAsk(task) {
		return askLine{}
	}
	line, _, _ := strings.Cut(strings.TrimSpace(task), "\n")
	words, skip := shellFields(line), 4 // past "Run: claude-code agents ask", which opensWithAsk matched
	if words[2] == "--remote" {
		skip = 6 // and the global "--remote <host>"
	}
	words = words[min(skip, len(words)):]
	l := askLine{agent: flagValue(words, "agent"), model: flagValue(words, "model")}
	value := flagValue(words, "overlay")
	if value == "" || value == "-" { // "" would read $COWORKER_OVERLAY, "-" the hook's stdin: no prediction
		return l
	}
	if cfg, err := agentconfig.LoadOverlay(value, strings.NewReader("")); err == nil { // unreadable: no prediction
		l.cfg = cfg
	}
	return l
}

// workAgent is the external agent the line's --agent asks for ("" = Claude); with no --agent, the leaf type's.
func (l askLine) workAgent(st, task string) string {
	switch {
	case l.agent == "":
		return workAgent(st, task)
	case externalTypes[l.agent]:
		return l.agent
	}
	return ""
}

// flagValue is the last -name/--name value before the first positional word or "--", as Go's flag package parses.
func flagValue(words []string, name string) string {
	v := ""
	for i := 0; i < len(words) && isFlag(words[i]); i++ {
		n, val, hasVal := strings.Cut(strings.TrimPrefix(words[i][1:], "-"), "=")
		if !hasVal && takesValue(words, i+1) {
			val, hasVal, i = words[i+1], true, i+1
		}
		if n == name && hasVal {
			v = val
		}
	}
	return v
}

// isFlag is a word Go's flag package reads as a flag: "-x" or "--x", never "--" or a positional word.
func isFlag(w string) bool { return w != "--" && len(w) >= 2 && w[0] == '-' }

// takesValue is words[i] being the previous flag's value: a word or "-" (stdin); a flag means that one was boolean.
func takesValue(words []string, i int) bool {
	return i < len(words) && (words[i] == "-" || !strings.HasPrefix(words[i], "-"))
}

// shellWordRe matches one shell word, shellPieceRe a quoted or escaped piece of it, shellEscRe an escape in "…".
var (
	shellWordRe = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`(?:[^\s'"\\]+|\\.?|'[^']*'?|"(?:[^"\\]|\\.)*"?)+`)
	})
	shellPieceRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`\\.?|'[^']*'?|"(?:[^"\\]|\\.)*"?`) })
	shellEscRe   = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`\\(.)`) })
)

// shellFields splits a command line into the words a POSIX shell passes: quotes group and are removed, \ escapes.
func shellFields(line string) []string {
	words := shellWordRe().FindAllString(line, -1)
	for i, w := range words {
		words[i] = shellPieceRe().ReplaceAllStringFunc(w, unquotePiece)
	}
	return words
}

// unquotePiece is the text of one escaped (\x), single-quoted or double-quoted piece of a shell word.
func unquotePiece(p string) string {
	switch p[0] {
	case '\\':
		return p[1:]
	case '\'':
		return strings.TrimSuffix(p[1:], "'")
	}
	return shellEscRe().ReplaceAllString(strings.TrimSuffix(p[1:], `"`), "$1")
}

// directAsk is a brief that opens with its own ask command (the /<agent> and /coworker commands): never rewritten.
func directAsk(task string) bool {
	return opensWithAsk(task) && briefProfile(task) == "" && briefCoworker(task) == ""
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
	if agent == "" {
		return "name one roster agent"
	}
	return fmt.Sprintf("use %q", agent)
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

// rewrite applies the Profile: line, the Coworker: line and the label in order; the first deny wins.
func (e *effortPlan) rewrite(promptField, task string, auto bool) *core.Result {
	if deny := e.profile(promptField, task); deny != nil {
		return deny
	}
	if deny := e.coworker(promptField, task); deny != nil {
		return deny
	}
	return e.label(task, auto)
}

// coworker applies a brief's Coworker: line; a non-nil result denies the dispatch.
func (e *effortPlan) coworker(promptField, task string) *core.Result {
	note, deny := applyCoworker(e.run, e.updated, promptField, task)
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

// labelModel is the one precedence: Run: line model, Coworker pick, Profile: model, work agent's cell, own model.
func labelModel(run *dispatchRun, updated map[string]any, st, task string) string {
	line := run.opening(task)
	if m := cmp.Or(line.model, line.cfg.Model); m != "" {
		return m // the brief's own ask command names the model: the label says what runs
	}
	agent := line.workAgent(st, task)
	confine := labelConfinement(coworkerApplies(st, task), task, line.cfg)
	switch tier := labelTier(run, updated, st); {
	case confine != "":
		return coworkerPick(run, confine, cmp.Or(agent, agentid.Claude), tier)
	case profileApplies(st, task):
		return labelProfileModel(run, briefProfile(task), tier)
	case agent != "":
		return labelCell(run, agent, tier)
	}
	return ownModel(run, updated, st)
}

// labelTier is the alias the pick starts from: the dispatch's model, else the agent file's, else sonnet.
func labelTier(run *dispatchRun, updated map[string]any, st string) string {
	tier, _ := updated["model"].(string)
	if tier == "" || tier == "inherit" {
		tier = run.rosterModel(st)
	}
	if !slices.Contains(models.AliasNames, tier) {
		return models.Sonnet
	}
	return tier
}

// labelConfinement is the provider[:profile] the label picks inside: a Coworker: line's, else the Run: line config's.
func labelConfinement(coworker bool, task string, cfg agentconfig.Overlay) string {
	if coworker {
		if p, _ := coworkerWorkspace(task); p != "" {
			return router.JoinCoworker(router.RouteDatabricks, p)
		}
		return ""
	}
	if cfg.Provider == router.RouteDatabricks && (cfg.Profile == "" || cfg.Profile == "auto") {
		return "" // auto resolves at run time: the pin-based label stands
	}
	return cfg.Confinement()
}

// coworkerPick is labelCell inside confinement confine: the env ask exports for that overlay, restored after.
func coworkerPick(run *dispatchRun, confine, agent, tier string) string {
	env := map[string]string{router.EnvCoworker: confine}
	if p, ok := strings.CutPrefix(confine, router.RouteDatabricks+":"); ok {
		env[providers.EnvSessionProfile] = p
	}
	for k, v := range env {
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
	profile, provider := router.ConfineLead(agent, "", "", alias) // the lead gateway and ask fill under a confinement
	q := agentpkg.RouteReq{Agent: agent, Model: alias, Profile: profile, Provider: provider,
		Fallback: router.FallbackAll, Preview: true}
	_, res, err := agentpkg.Resolve(context.Background(), q)
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

// briefProfile is the profile a `Profile:` line in the brief's header block names, or "".
func briefProfile(task string) string {
	if m := profileLineRe().FindStringSubmatch(briefHeader(task)); m != nil {
		return strings.TrimRight(m[1], ".") // "Profile: dev." ends a sentence, not the name
	}
	return ""
}

// briefAgents are the agents the `Agent:` line names, claude when the line is absent; nil for in-context.
func briefAgents(task string) []string {
	m := agentLineRe().FindStringSubmatch(briefHeader(task))
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
	m := agentLineRe().FindStringSubmatch(briefHeader(task))
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
	agents, st := briefAgents(task), fmt.Sprint(updated["subagent_type"])
	if coworkerApplies(st, task) && router.ArmProfile() == "" {
		return "" // the Coworker: step writes the one Run: line, or blocks when the two differ
	}
	if externalTypes[st] && !agentLineRe().MatchString(briefHeader(task)) {
		agents = []string{st} // with no Agent: line an external agent type runs as itself, as under Coworker:
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

// coworkerLineRe matches a brief's `Coworker:` line.
var (
	coworkerLineRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?m)^[ \t]*Coworker:[ \t]*(\S+)`) })
	blankLineRe    = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`\n[ \t\r]*\n\s*`) })
	profileNameRe  = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[A-Za-z0-9._-]+$`) })
)

// briefHeader is the brief's header block: the lines before its first blank line, past an opening Run: line.
func briefHeader(task string) string {
	paras := blankLineRe().Split(strings.TrimSpace(task), 3)
	if len(paras) > 1 && strings.HasPrefix(paras[0], "Run:") && !strings.Contains(paras[0], "\n") {
		return paras[0] + "\n" + paras[1]
	}
	return paras[0]
}

// briefCoworker is the value of a `Coworker:` line in the brief's header block, or "".
func briefCoworker(task string) string {
	if m := coworkerLineRe().FindStringSubmatch(briefHeader(task)); m != nil {
		return strings.TrimRight(m[1], ".")
	}
	return ""
}

// coworkerApplies is true when a brief's Coworker: reaches a leaf of type st: Explore, Plan and in-context skip it.
func coworkerApplies(st, task string) bool {
	return briefCoworker(task) != "" && (!readOnlyTypes[st] || externalTypes[st]) && !inContextBrief(task)
}

// coworkerWorkspace is the workspace a brief's Coworker: line names (bare: the armed one's, else the session profile).
func coworkerWorkspace(task string) (string, error) {
	mode, p, err := router.ParseCoworker(briefCoworker(task))
	switch {
	case err != nil:
		return "", err
	case mode != router.RouteDatabricks:
		return "", fmt.Errorf("brief Coworker: %s: only databricks[:<profile>] is a brief coworker", mode)
	case p == "":
		p = cmp.Or(router.ArmProfile(), providers.SessionProfile())
	}
	if p == "" {
		return "", errors.New("brief Coworker: databricks needs a workspace: databricks:<profile>")
	}
	if !profileNameRe().MatchString(p) { // p lands in a command a leaf runs in a shell
		return "", fmt.Errorf("brief Coworker: profile %q: only letters, digits, '.', '_' and '-' are allowed", p)
	}
	return p, nil
}

// servingRow refuses a profile coworker databricks:<p> cannot run on; tests stub it.
var servingRow = func(run *dispatchRun, p string) error {
	reg, err := run.registry()
	if err != nil {
		return nil // fail open, like profileNames: ask itself refuses the row
	}
	return reg.ServingRow(p)
}

// applyCoworker prepends `agents ask --overlay '<doc>'` to a Coworker: dispatch; deny blocks a bad or conflicting one.
func applyCoworker(run *dispatchRun, updated map[string]any, promptField, task string) (note string, deny *core.Result) {
	block := func(msg string) (string, *core.Result) {
		return "", &core.Result{Decision: core.Block, Reason: "dispatch-effort: " + msg}
	}
	st, _ := updated["subagent_type"].(string)
	if !coworkerApplies(st, task) {
		return "", nil
	}
	p, err := coworkerWorkspace(task)
	if err != nil {
		return block(err.Error())
	}
	own, arm := run.opening(task).cfg.Confinement(), router.ArmProfile() // own: the Run: line's confinement, never stacked
	overlay := "--overlay " + textutil.ShellQuote(agentconfig.InlineOverlay(router.RouteDatabricks, p))
	if msg := coworkerConflict(task, p, arm, own, overlay); msg != "" {
		return block(msg)
	}
	if arm == p || own != "" {
		return "", nil
	}
	if msg := coworkerRowErr(run, p); msg != "" {
		return block(msg)
	}
	cmd := "claude-code agents ask --agent " + coworkerAgent(st, task) + " " + overlay
	prompt, _ := updated[promptField].(string)
	updated[promptField] = "Run: " + cmd + " (coworker databricks:" + p + "; ask resolves host and auth)\n\n" + prompt
	return "dispatch-effort: Coworker: databricks:" + p + " — leaf must run `" + cmd + "`.", nil
}

// coworkerConflict names what Coworker: databricks:<p> conflicts with: the armed workspace, Profile: or the Run: line.
func coworkerConflict(task, p, arm, own, overlay string) string {
	switch profile := briefProfile(task); {
	case cmp.Or(arm, p) != p:
		return "Coworker: databricks:" + p + " conflicts with databricks:" + arm
	case cmp.Or(profile, p) != p:
		return "Coworker: databricks:" + p + " conflicts with Profile: " + profile
	case own != "" && own != router.RouteDatabricks+":"+p:
		return "Coworker: databricks:" + p + " conflicts with the brief's own overlay " + own
	case own+arm == "" && opensWithAsk(task):
		return "Coworker: databricks:" + p + ": the brief already opens with its own Run: line; put " + overlay +
			" on that line, else drop the Coworker: line"
	}
	return ""
}

// coworkerRowErr is why workspace p cannot run a coworker (unknown, or no serving row); "" when it can.
func coworkerRowErr(run *dispatchRun, p string) string {
	if names, ok := profileNames(run); ok && !slices.Contains(names, p) {
		return fmt.Sprintf("overlay databricks:%s: no such profile", p)
	}
	if err := servingRow(run, p); err != nil {
		return err.Error()
	}
	return ""
}

// coworkerAgent is the agent a Coworker: dispatch asks for: the work agent, else claude.
func coworkerAgent(st, task string) string {
	return cmp.Or(workAgent(st, task), agentid.Claude)
}
