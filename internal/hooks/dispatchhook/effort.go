// `dispatch-effort`: PreToolUse on spawn/Workflow: model pin, recall, label; fails OPEN.

// Package dispatchhook is the dispatch group: effort, leaf lifecycle, hand-back checks and codex routing.
package dispatchhook

import (
	"cmp"
	"context"
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	agentpkg "github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/dev/transcript"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/routing/agentid"
	"github.com/edotau/claude-code/internal/routing/providers"
	"github.com/edotau/claude-code/internal/routing/router"
)

// agentLineRe matches a brief's `Agent:` line (read from briefHeader only); agentNameRe the words in it.
var (
	agentLineRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?m)^[ \t]*Agent:[ \t]*(.+)$`) })
	agentNameRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`[a-z]+`) })
	codemodeRe  = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`(?mi)^[ \t]*Codemode:[ \t]*(on|only|off)[ \t\r]*$`)
	})
	blankLineRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`\n[ \t\r]*\n\s*`) })
)

const jevHookBudget = 450 * time.Millisecond

type jevHookKey struct{}

// withJevHookBudget caps Jev only; recall and other hook operations retain their caller context.
func withJevHookBudget(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx.Value(jevHookKey{}) != nil {
		return ctx, func() {}
	}
	jevCtx, cancel := context.WithTimeout(ctx, jevHookBudget)
	return context.WithValue(ctx, jevHookKey{}, jevCtx), cancel
}

func hookJevContext(ctx context.Context) context.Context {
	if jevCtx, ok := ctx.Value(jevHookKey{}).(context.Context); ok {
		return jevCtx
	}
	return ctx
}

// dispatchRun memoizes one hook run's agent-file and provider-registry reads; session is the session's model id.
type dispatchRun struct {
	session  func() string
	roster   map[string]string
	efforts  map[string]string // frontmatter effort by agent name, filled by the same read as roster
	registry func() (*providers.Registry, error)
	profile  string // `agents label --profile`: the ask --profile
	model    string // `agents label --model`: the ask --model, so an alias is not read as the leaf's own model
	hops     map[hopKey]hopResult
}

// hopKey and hopResult memoize runQuery; an external hop is reused only while its eligibility generation holds.
type hopKey struct {
	q    agentpkg.ModelQuery
	hint string
}

type hopResult struct {
	hop  agentpkg.ModelQuery
	note string
	err  error
	gen  router.Generation
}

func (r *dispatchRun) runHop(ctx context.Context, reg *providers.Registry, q agentpkg.ModelQuery, hint string) (agentpkg.ModelQuery, string, error) {
	k := hopKey{q, hint}
	// Claude's query never reads eligibility: namesRoute returns it unchanged
	if h, ok := r.hops[k]; ok && (q.Agent == "claude" || h.gen.Holds(time.Now())) {
		return h.hop, h.note, h.err
	}
	var gen router.Generation
	if q.Agent != "claude" {
		gen = router.TakeGeneration(time.Now())
	}
	hop, note, err := runQuery(ctx, reg, q, hint)
	if r.hops == nil {
		r.hops = map[hopKey]hopResult{}
	}
	r.hops[k] = hopResult{hop, note, err, gen}
	return hop, note, err
}

// Per-run read seams: tests count them; production reads the roster file and the provider registry.
var (
	readAgentFile = os.ReadFile
	loadRegistry  = providers.Load
)

func newDispatchRun(session func() string) *dispatchRun {
	return &dispatchRun{session: session, roster: map[string]string{}, efforts: map[string]string{}, registry: sync.OnceValues(loadRegistry)}
}

func (r *dispatchRun) rosterModel(name string) string {
	m, ok := r.roster[name]
	if !ok {
		m, r.efforts[name] = rosterEntry(name)
		r.roster[name] = m
	}
	return m
}

func (r *dispatchRun) rosterEffort(name string) string {
	r.rosterModel(name)
	return r.efforts[name]
}

// agentEffortResult is the pin step: a spawn gets one UpdatedInput (pin, recall, label), a Workflow its lint result.
func agentEffortResult(ctx context.Context, in *core.Input) core.Result {
	return effortFor(ctx, in, modeFor(in))
}

func effortFor(ctx context.Context, in *core.Input, mode spawnMode) core.Result {
	if !core.SpawnTools[in.ToolName] {
		return core.Result{}
	}
	ctx, cancel := withJevHookBudget(ctx)
	defer cancel()
	fixType := effortFixType(ctx, in)
	if fixType != "" && mode == modeDeferred {
		mode = modeToday // the host validates the type before agent.spawn, so only this hook can retype
	}
	if fixType != "" && mode == modeQuery {
		fixType = "" // the mod only sees a type the host already accepted
	}
	// updatedInput REPLACES the whole tool_input record, so every original key must be echoed back.
	updated := in.CloneToolInput()
	if updated == nil {
		return core.Result{}
	}

	promptField, task := spawnTask(in)

	session := func() string { u, _ := transcript.LastUsage(in.TranscriptPath); return u.Model }
	e := &effortPlan{updated: updated, run: newDispatchRun(session), mode: mode}
	e.retype(in, fixType)
	task = e.keepPiAgent(in, fixType, promptField, task)
	if !explicitModel(in) && !fixedModelTypes[in.Nested("tool_input", "subagent_type")] {
		go agentpkg.TaskTierCtx(hookJevContext(ctx), task)
	}
	return e.finish(ctx, in, promptField, task)
}

// finish runs the recall, model pin and label steps and renders the result.
func (e *effortPlan) finish(ctx context.Context, in *core.Input, promptField, task string) core.Result {
	st := in.Nested("tool_input", "subagent_type")
	extraNote := core.JoinContext(e.recall(ctx, in, promptField, task, true), e.benchNote(in.CWD, task),
		degradedNote(task, briefPlanTask(task)))
	if e.mode == modeDeferred {
		return core.Result{Context: extraNote}
	}
	if !fixedModelTypes[st] {
		pinNote, pinMsg := pinModelCtx(ctx, e.run, in, e.updated, task)
		e.fix(pinNote)
		e.systemMessage = strings.TrimSpace(e.systemMessage + " " + pinMsg)
		e.changed = e.changed || pinNote != ""
		e.label(ctx, task)
	}
	e.note = strings.TrimSpace(e.note + "\n" + extraNote)
	if in.Nested("permission_mode") == "auto" && e.changed && e.mode != modeQuery && !e.retyped(st) {
		// auto mode gives no verdict on a hook-changed input: send the fixes as context, leave the call as sent
		fields, _ := json.Marshal(changedFields(in.ToolInput(), e.updated))
		e.note = strings.TrimSpace(e.note + "\ndispatch-effort: auto mode leaves this call unchanged; resend with " + string(fields))
		e.changed, e.systemMessage = false, ""
	}
	return e.result()
}

// keepPiAgent adds an `Agent: pi` header line to a brief retyped from a Pi type to claude, so the leaf still runs Pi.
func (e *effortPlan) keepPiAgent(in *core.Input, fixType, promptField, task string) string {
	st := in.Nested("tool_input", "subagent_type")
	piType := agentid.AgentOfType(st) == agentid.Pi || staleTypeTarget(st) != ""
	if fixType != agentid.Claude || !piType || promptField != "prompt" || agentLineRe().MatchString(briefHeader(task)) {
		return task
	}
	task = "Agent: " + agentid.Pi + "\n" + task
	e.updated[promptField] = task
	e.systemMessage = strings.TrimSpace(e.systemMessage + " Added `Agent: pi` to the brief so the claude leaf runs a plain Pi ask.")
	return task
}

func (e *effortPlan) retyped(st string) bool {
	got, _ := e.updated["subagent_type"].(string)
	return got != st
}

// effortFixType is the type to retype to: a stale name's, code mode's, or confident Open-Jev's; "" for none.
func effortFixType(ctx context.Context, in *core.Input) string {
	st, prompt := in.Nested("tool_input", "subagent_type"), in.Nested("tool_input", "prompt")
	if to := staleTypeTarget(st); to != "" { // a bare "pi" is the coworker type now
		return cmp.Or(codemodeTarget(to, prompt), to)
	}
	if to := codemodeTarget(st, prompt); to != "" {
		return to
	}
	if st == "" && !explicitModel(in) && !agentLineRe().MatchString(briefHeader(prompt)) {
		if to := agentid.SubagentType(agentpkg.JevAgentCtx(hookJevContext(ctx), prompt)); to != "" && slices.Contains(dispatchTypes(), to) {
			return to
		}
	}
	return ""
}

func spawnTask(in *core.Input) (field, task string) {
	if task = in.Nested("tool_input", "prompt"); task != "" {
		return "prompt", task
	}
	return "description", in.Nested("tool_input", "description")
}

type effortPlan struct {
	run           *dispatchRun
	updated       map[string]any
	note          string
	systemMessage string
	changed       bool
	mode          spawnMode
}

// recall appends the memory-recall block to the prompt, or returns it as a note; the mod's query skips it (it stamps).
func (e *effortPlan) recall(ctx context.Context, in *core.Input, promptField, task string, asNote bool) string {
	if e.mode == modeQuery {
		return ""
	}
	suffix, note := briefRecall(ctx, in.SessionID, task, asNote)
	if suffix != "" {
		e.updated[promptField] = task + suffix
		e.changed = true
	}
	return note
}

type recallKey struct{}

type recallPrefetch struct {
	task string
	done chan struct{}
	text string
}

// prefetchRecall starts the read-only recall search for a spawn's task so it overlaps the cap check.
func prefetchRecall(ctx context.Context, in *core.Input) context.Context {
	if !core.SpawnTools[in.ToolName] {
		return ctx
	}
	_, task := spawnTask(in)
	p := &recallPrefetch{task: task, done: make(chan struct{})}
	go func() {
		defer close(p.done)
		p.text, _ = memory.AgentRecall(ctx, in.SessionID, task)
	}()
	return context.WithValue(ctx, recallKey{}, p)
}

// waitRecall joins the prefetch so no recall-index write outlives the hook's return.
func waitRecall(ctx context.Context) {
	if p, ok := ctx.Value(recallKey{}).(*recallPrefetch); ok {
		<-p.done
	}
}

func recallSearch(ctx context.Context, session, task string) string {
	if p, ok := ctx.Value(recallKey{}).(*recallPrefetch); ok && p.task == task {
		<-p.done
		return p.text
	}
	text, _ := memory.AgentRecall(ctx, session, task)
	return text
}

// briefRecall is a dispatch's memory-recall block, as text to append to its brief or, with asNote, a context note.
func briefRecall(ctx context.Context, session, task string, asNote bool) (suffix, note string) {
	recallBlock := recallSearch(ctx, session, task)
	if recallBlock == "" {
		return "", ""
	}
	if !asNote {
		memory.LogBriefRecall(session, recallBlock)
		return recallBlock, ""
	}
	if recallBlock = freshPointers(session, recallBlock); recallBlock == "" {
		return "", ""
	}
	memory.LogBriefRecall(session, recallBlock)
	return "", "memory-recall for this dispatch (paste into the brief if relevant):" + recallBlock
}

// freshPointers drops pointer lines already noted this session and records the rest; "" when none are new.
func freshPointers(session, block string) string {
	if session == "" {
		return block
	}
	var out []string
	fresh := false
	for l := range strings.SplitSeq(block, "\n") {
		if ptr, ok := strings.CutPrefix(l, "- "); ok {
			key, _, _ := strings.Cut(ptr, " ")
			if first, _ := core.SeenAdd("memory/dispatch-recall-seen", session, key); !first {
				continue
			}
			fresh = true
		}
		out = append(out, l)
	}
	if !fresh {
		return ""
	}
	return strings.Join(out, "\n")
}

// label prefixes the description with the run's model, on every brief: only the description changes.
func (e *effortPlan) label(ctx context.Context, task string) {
	changed, note := applyLabel(ctx, e.run, e.updated, task)
	e.changed = changed || e.changed
	e.fix(note)
}

// fix adds one note line per fix; every fix of a call rides the same updatedInput, so auto mode resends once.
func (e *effortPlan) fix(note string) {
	if note != "" {
		e.note = strings.TrimSpace(e.note + "\n" + note)
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
