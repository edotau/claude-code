// `dispatch-effort`: PreToolUse on spawn/Workflow: type deny, model pin, recall, label; fails OPEN.
package agents

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	agentpkg "github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/agentconfig/project"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hooks/quality"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/transcript"
)

// agentLineRe matches a brief's `Agent:` line (read from briefHeader only); agentNameRe the words in it.
var (
	agentLineRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?m)^[ \t]*Agent:[ \t]*(.+)$`) })
	agentNameRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`[a-z]+`) })
	blankLineRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`\n[ \t\r]*\n\s*`) })
)

// dispatchRun memoizes one hook run's agent-file and provider-registry reads; session is the session's model id.
type dispatchRun struct {
	session  func() string
	roster   map[string]string
	efforts  map[string]string // frontmatter effort by agent name, filled by the same read as roster
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
	return &dispatchRun{session: session, roster: map[string]string{}, efforts: map[string]string{}, registry: sync.OnceValues(loadRegistry)}
}

// rosterModel is the named agent's frontmatter model, read once per run.
func (r *dispatchRun) rosterModel(name string) string {
	m, ok := r.roster[name]
	if !ok {
		m, r.efforts[name] = rosterEntry(name)
		r.roster[name] = m
	}
	return m
}

// rosterEffort is the named agent's frontmatter effort, from the same cached read as rosterModel.
func (r *dispatchRun) rosterEffort(name string) string {
	r.rosterModel(name)
	return r.efforts[name]
}

// agentEffortResult is the pin step: a spawn gets one UpdatedInput (pin, recall, label), a Workflow its lint result.
func agentEffortResult(ctx context.Context, in *core.Input) core.Result {
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
	direct := opensWithAsk(task)
	auto := in.Nested("permission_mode") == "auto" || direct
	session := func() string { u, _ := transcript.LastUsage(in.TranscriptPath); return u.Model }
	e := &effortPlan{updated: updated, run: newDispatchRun(session)}
	if deny := preflightBlock(ctx, e.run, task); deny != nil {
		return *deny
	}
	recallNote := e.recall(ctx, in, promptField, task, auto)
	if !direct {
		e.note, e.systemMessage = pinModel(e.run, in, updated, task)
		e.changed = e.changed || e.note != ""
	}
	if deny := e.label(ctx, task, auto); deny != nil {
		return *deny
	}
	e.note = strings.TrimSpace(e.note + "\n" + recallNote)
	return e.result()
}

// openingAskWords uses the receipt grammar for a leading Run command, retaining its flags and quoted prompt.
func openingAskWords(task string) []string {
	line, _, _ := strings.Cut(strings.TrimSpace(task), "\n")
	command, ok := strings.CutPrefix(line, "Run:")
	if !ok {
		return nil
	}
	match := askCmdRe().FindStringSubmatchIndex(command)
	if match == nil || match[0] != 0 {
		return nil
	}
	words := shellFields(command[match[2]:])
	if len(words) < 3 {
		return nil
	}
	verb := 1
	if words[1] == "--remote" {
		verb = 3
	}
	if len(words) <= verb+1 || words[verb+1] != "ask" {
		return nil
	}
	return words
}

// opensWithAsk is a brief opening with its own ask command: never rewritten, only labelled.
func opensWithAsk(task string) bool { return len(openingAskWords(task)) > 0 }

// askLine is a brief's opening ask command: its --agent, --model, last word (the prompt), and --overlay document.
type askLine struct {
	agent, model, prompt string
	profile, fallback    string // --profile and --fallback as typed
	local                bool   // no flag runs it off this machine or off the line's own route: preflight predictable
	cfg                  agentconfig.Overlay
}

// offRoute are ask flags taking the run to another host, lead, record or batch: the hook predicts no preflight.
var offRoute = []string{"agents", "native", "provider", "host", "host-bin", "retry", "retry-agent", "resume", "live",
	"log", "job", "jobs", "batch", "follow"}

// opening is task's opening ask command, parsed once per run; zero for a brief that does not open with one.
func (r *dispatchRun) opening(ctx context.Context, task string) askLine {
	if r.line == nil || r.lineTask != task {
		l := parseAskLine(ctx, task)
		r.line, r.lineTask = &l, task
	}
	return *r.line
}

// parseAskLine reads the flags a shell would see on the opening ask command; text inside a quoted task is no flag.
func parseAskLine(ctx context.Context, task string) askLine {
	words := openingAskWords(task)
	if len(words) == 0 {
		return askLine{}
	}
	verb := 1
	if words[1] == "--remote" {
		verb = 3
	}
	agent := strings.TrimPrefix(words[verb], "agents")
	words = words[verb+2:]
	l := askLine{agent: cmp.Or(flagValue(words, "agent"), agent), model: flagValue(words, "model"),
		profile: flagValue(words, "profile"), fallback: flagValue(words, "fallback"), local: verb == 1 && !hasFlag(words, offRoute)}
	if n := len(words); n > 0 && !isFlag(words[n-1]) {
		l.prompt = words[n-1]
	}
	value := flagValue(words, "overlay")
	if value == "" || value == "-" { // "" would read $COWORKER_OVERLAY, "-" the hook's stdin: no prediction
		return l
	}
	if cfg, err := agentconfig.LoadOverlayContext(ctx, value, strings.NewReader("")); err == nil { // unreadable: no prediction
		l.cfg = cfg
	}
	return l
}

// preflightBlock denies a Run: line `agents ask` refuses on local cached state alone; every doubt lets it through.
func preflightBlock(ctx context.Context, run *dispatchRun, task string) *core.Result {
	l := run.opening(ctx, task)
	agent := cmp.Or(l.agent, agentid.Claude)
	if !l.local || l.cfg.Host != "" || l.cfg.Remote != "" || !agentid.Known(agent) {
		return nil
	}
	if provider, _, err := router.CoworkerContext(ctx); provider != "" || err != nil {
		return nil // nested in a confinement: ask's own leave rules decide
	}
	reg, err := run.registry()
	if err != nil {
		return nil
	}
	if err := l.refusal(reg, agent); err != nil {
		return &core.Result{Decision: core.Block, Reason: "dispatch-effort: " + err.Error()}
	}
	return nil
}

// refusal is ask's error for l's Databricks row and model, judged on reg and the row's fresh listing; nil passes.
func (l askLine) refusal(reg *providers.Registry, agent string) error {
	none := sync.OnceValue(func() bool {
		f := cmp.Or(l.fallback, l.cfg.Fallback)
		return f == router.FallbackNone || f == "" && router.DefaultFallback() == router.FallbackNone
	})
	p, model, err := l.target(reg, none)
	if err != nil || p == nil || p.Preset != providers.PresetDatabricks || model == "" {
		return err
	}
	if f := agentpkg.FreshModelFinding(agent, p, model, time.Now()); f != nil && none() {
		return &agentpkg.PreflightError{Finding: *f}
	}
	return nil
}

// target is the row and bare model l's ask preflights; err is ask's refusal of the row, a nil row no prediction.
func (l askLine) target(reg *providers.Registry, none func() bool) (*providers.Provider, string, error) {
	model, typed := l.typedModel(reg)
	if l.cfg.Provider == router.RouteDatabricks {
		return l.overlayTarget(reg, typed, model)
	}
	name := cmp.Or(l.profile, typed)
	if l.cfg.Provider != "" || name == "" || name == agentpkg.LeadSession || (typed != "" && name != typed) {
		return nil, "", nil
	}
	p, err := reg.Get(name)
	if err != nil && !none() {
		return nil, "", nil // a --profile lead with a fallback: ask routes on, it does not refuse
	}
	return p, model, err
}

// typedModel is the line's model with a leading `<row>:` cut off, and that row ("" when it names no row of reg).
func (l askLine) typedModel(reg *providers.Registry) (model, typed string) {
	model = cmp.Or(l.model, l.cfg.Model)
	if prof, bare, ok := strings.Cut(model, ":"); ok && reg.Providers[prof] != nil {
		return bare, prof
	}
	return model, ""
}

// overlayTarget is target under overlay databricks:<p>; auto, bare, or a flag naming another row is not predicted.
func (l askLine) overlayTarget(reg *providers.Registry, typed, model string) (*providers.Provider, string, error) {
	p := l.cfg.Profile
	if p == "" || p == router.AutoArm || cmp.Or(l.profile, p) != p || cmp.Or(typed, p) != p {
		return nil, "", nil
	}
	if err := reg.ServingRow(p); err != nil {
		return nil, "", err
	}
	return reg.Providers[p], model, nil
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

// hasFlag reports any of names set before the first positional word or "--", as flagValue walks them.
func hasFlag(words, names []string) bool {
	for i := 0; i < len(words) && isFlag(words[i]); i++ {
		n, _, hasVal := strings.Cut(strings.TrimPrefix(words[i][1:], "-"), "=")
		if slices.Contains(names, n) {
			return true
		}
		if !hasVal && takesValue(words, i+1) {
			i++
		}
	}
	return false
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

// builtinTypes are the harness built-in Claude-run leaf types with no agents/<name>.md that a dispatch may use.
var builtinTypes = []string{"Explore", "general-purpose"}

// listAgents loads the agent definitions; a seam so tests count or fail the read.
var listAgents = project.DiscoverAgents

// dispatchTypes is every allowed subagent_type, sorted; an unreadable or empty agents/ falls back to the roster.
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
func (e *effortPlan) recall(ctx context.Context, in *core.Input, promptField, task string, auto bool) string {
	recallBlock, _ := memory.AgentRecall(ctx, in.SessionID, in.CWD, task)
	if recallBlock == "" {
		return ""
	}
	if auto {
		if recallBlock = freshPointers(in.SessionID, recallBlock); recallBlock == "" {
			return ""
		}
		return "memory-recall for this dispatch (paste into the brief if relevant):" + recallBlock
	}
	e.updated[promptField] = task + recallBlock
	e.changed = true
	return ""
}

// freshPointers drops pointer lines already noted this session and records the rest; "" when none are new.
func freshPointers(session, block string) string {
	id := filepath.Base(session)
	if session == "" || id == "." || id == string(filepath.Separator) {
		return block
	}
	path := filepath.Join(memory.StateDir(), "dispatch-recall-seen", id)
	data, _ := os.ReadFile(path)
	seen := strings.Fields(string(data))
	var out, fresh []string
	for l := range strings.SplitSeq(block, "\n") {
		if ptr, ok := strings.CutPrefix(l, "- "); ok {
			key, _, _ := strings.Cut(ptr, " ")
			if slices.Contains(seen, key) {
				continue
			}
			fresh = append(fresh, key)
		}
		out = append(out, l)
	}
	if len(fresh) == 0 {
		return ""
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	_ = appendSeen(path, fresh)
	return strings.Join(out, "\n")
}

// appendSeen adds one id per line to the session's dispatch-recall seen file.
func appendSeen(path string, ids []string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(strings.Join(ids, "\n") + "\n")
	return err
}

// label prefixes the description with the run's model: a rewrite, or in auto mode (rewrites rejected) a one-time deny.
func (e *effortPlan) label(ctx context.Context, task string, auto bool) *core.Result {
	if !auto {
		changed, _ := applyLabel(ctx, e.run, e.updated, task)
		e.changed = changed || e.changed
		return nil
	}
	probe := maps.Clone(e.updated)
	if _, canonical := applyLabel(ctx, e.run, probe, task); canonical { // a right prefix passes; case alone never blocks
		return nil
	}
	return &core.Result{Decision: core.Block, Reason: fmt.Sprintf("dispatch-effort: the description must start with the model the "+
		"dispatch runs on. Resend this dispatch with description %q (or print the prefix with `claude-code agents label <type>`).", probe["description"])}
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
func applyLabel(ctx context.Context, run *dispatchRun, updated map[string]any, task string) (changed, canonical bool) {
	st, _ := updated["subagent_type"].(string)
	orig, _ := updated["description"].(string)
	if strings.TrimSpace(orig) == "" {
		return false, true
	}
	label := labelModel(ctx, run, updated, st, task)
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

// pinModel fills an unset updated["model"] from the classifier; returns the note and visible reason ("" = no pin).
func pinModel(run *dispatchRun, in *core.Input, updated map[string]any, task string) (note, systemMessage string) {
	// never override an explicit model or a roster pin; `inherit` means unset (honoring it ran on the parent's model)
	if explicitModel(in) || task == "" || agentPinsModel(run, in.Nested("tool_input", "subagent_type")) {
		return "", ""
	}
	tier, signal := router.EffortTierSignal(task)
	model := models.AliasModel(tier)
	updated["model"] = model
	effort := ""
	if set := pinEffort(run, in, updated, tier); set != "" {
		effort = " effort=" + set
	}
	return fmt.Sprintf("dispatch-effort: dispatch model unset and the task classifies %s — pinned model=%s%s"+
			" (set model explicitly to override).", tier, model, effort),
		fmt.Sprintf("dispatch-effort: %s → %s%s (%s)", tier, model, effort, signal)
}

// pinEffort sets updated["effort"] to tier unless the call or agent file sets one; returns what it set, else "".
func pinEffort(run *dispatchRun, in *core.Input, updated map[string]any, tier string) string {
	if strings.TrimSpace(in.Nested("tool_input", "effort")) != "" || run.rosterEffort(in.Nested("tool_input", "subagent_type")) != "" {
		return ""
	}
	updated["effort"] = tier
	return tier
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

// rosterEntry is the named harness agent's frontmatter model and effort ("" for a built-in or a path-shaped name).
func rosterEntry(name string) (model, effort string) {
	file, ok := agentFile(name)
	if !ok {
		return "", ""
	}
	raw, err := readAgentFile(file)
	if err != nil {
		return "", ""
	}
	fm, _ := textutil.ParseFrontmatter(string(raw))
	model, _ = fm["model"].(string)
	effort, _ = fm["effort"].(string)
	return model, effort
}

// agentPinsModel reports whether the named subagent's frontmatter sets any model, `inherit` included.
func agentPinsModel(run *dispatchRun, name string) bool {
	if name == "" {
		return false // no subagent_type = a fresh general-purpose agent, nothing to defer to
	}
	if _, ok := agentFile(name); !ok {
		return true // path-shaped input is not a roster name; refuse to touch the dispatch
	}
	return run.rosterModel(name) != "" // no roster file = a built-in (Explore/Plan), the classifier may apply
}

// labelModel is the one precedence: Run: line model, its overlay's pick, work agent's cell, own model.
func labelModel(ctx context.Context, run *dispatchRun, updated map[string]any, st, task string) string {
	line := run.opening(ctx, task)
	agent := line.workAgent(st, task)
	confine := labelConfinement(line.cfg)
	if m := cmp.Or(line.model, line.cfg.Model); m != "" {
		return explicitLabel(ctx, run, confine, cmp.Or(agent, agentid.Claude), m)
	}
	switch tier := pickTier(run, updated, st, agent, cmp.Or(line.prompt, task)); {
	case confine != "":
		return coworkerPick(ctx, run, confine, cmp.Or(agent, agentid.Claude), tier)
	case agent != "":
		return labelCell(ctx, run, agent, tier)
	}
	return ownModel(run, updated, st)
}

// explicitLabel is the ask's model as the run spells it inside confine (a Databricks endpoint name), else as typed.
func explicitLabel(ctx context.Context, run *dispatchRun, confine, agent, model string) string {
	if confine == "" {
		return model
	}
	return cmp.Or(coworkerPick(ctx, run, confine, agent, model), model)
}

// pickTier is the alias the run picks from: an external agent's ask classifies its prompt, as ask does with no --model.
func pickTier(run *dispatchRun, updated map[string]any, st, agent, task string) string {
	if agent != "" {
		return router.RuleClassifier{}.Alias(task)
	}
	return labelTier(run, updated, st)
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

// labelConfinement is the provider[:profile] the label picks inside: the Run: line overlay's.
func labelConfinement(cfg agentconfig.Overlay) string {
	if cfg.Provider == router.RouteDatabricks && (cfg.Profile == "" || cfg.Profile == "auto") {
		return "" // auto resolves at run time: the pin-based label stands
	}
	return cfg.Confinement()
}

// pickEnvMu serializes coworkerPick's env swap: router and preview readers still read the process env.
var pickEnvMu sync.Mutex

// coworkerPick is labelCell inside confinement confine: ask's values for it in the pick ctx and swapped into the env.
func coworkerPick(ctx context.Context, run *dispatchRun, confine, agent, tier string) string {
	provider, profile, _ := strings.Cut(confine, ":")
	env := proc.Values{agentconfig.OverlayVar: agentconfig.InlineOverlay(provider, profile)}
	if p, ok := strings.CutPrefix(confine, router.RouteDatabricks+":"); ok {
		env[providers.EnvSessionProfile] = p
	}
	pickEnvMu.Lock()
	defer pickEnvMu.Unlock() // deferred first, so it runs after every restoreEnv
	pick := pickContext(ctx, env)
	for k, v := range env {
		old, had := os.LookupEnv(k)
		_ = proc.Setenv(k, v)
		defer restoreEnv(k, old, had)
	}
	return labelCell(pick, run, agent, tier)
}

// pickContext is ctx's run values (else the process environment) with env laid over them.
func pickContext(ctx context.Context, env proc.Values) context.Context {
	base, ok := proc.ValuesFromContext(ctx)
	if !ok {
		base = proc.ValuesFromEnv(os.Environ())
	}
	return proc.WithValues(ctx, base.Merge(env))
}

func restoreEnv(k, old string, had bool) {
	if had {
		_ = proc.Setenv(k, old)
		return
	}
	_ = proc.Unsetenv(k)
}

// Label lookups are seams: tests stub them so a dispatch label never reads the live vault or route table.
var (
	labelCell = cellModelFor
	labelPick = previewModel
)

// cellModelFor is the model a leaf runs: the no-run preview's first candidate, else the route table's prediction.
func cellModelFor(ctx context.Context, run *dispatchRun, agent, alias string) string {
	pins, err := agentpkg.AgentPinsContext(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dispatch-effort label: %v\n", err)
		return ""
	}
	if slices.ContainsFunc(slices.Collect(maps.Values(pins)), func(s router.PinSpec) bool { return s.Model == router.DiscoveryModel }) {
		return "" // a discovery pick advances the rotation in quota.json; a label never writes state
	}
	if m := labelPick(ctx, agent, alias); m != "" {
		return m
	}
	return tableCellModel(run, agent, alias, pins[agent])
}

// previewModel is the preview's first candidate model without its `<profile>:` prefix; "" when nothing runs.
func previewModel(ctx context.Context, agent, alias string) string {
	profile, provider := router.ConfineLeadContext(ctx, agent, "", "", alias) // the lead gateway and ask fill under a confinement
	q := agentpkg.RouteReq{Agent: agent, Model: alias, Profile: profile, Provider: provider,
		Fallback: router.FallbackAll, Preview: true}
	_, res, err := agentpkg.Resolve(ctx, q)
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

// briefHeader is the brief's header block: the lines before its first blank line, past an opening Run: line.
func briefHeader(task string) string {
	paras := blankLineRe().Split(strings.TrimSpace(task), 3)
	if len(paras) > 1 && strings.HasPrefix(paras[0], "Run:") && !strings.Contains(paras[0], "\n") {
		return paras[0] + "\n" + paras[1]
	}
	return paras[0]
}
