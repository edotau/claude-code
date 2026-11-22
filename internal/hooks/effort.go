// `dispatch-effort`: PreToolUse on spawn/Workflow: type deny, model pin, recall, label; fails OPEN.
package hooks

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/transcript"
	"github.com/edotau/claude-code/internal/workflows"
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
	profile  string // `agents label --profile`: the ask --profile when the brief's Run: line names none
	model    string // `agents label --model`: the ask --model, so an alias is not read as the leaf's own model
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
	if in.ToolName == "Workflow" {
		return workflows.WorkflowResult(in)
	}
	if !core.SpawnTools[in.ToolName] {
		return core.Result{}
	}
	fixType, deny := effortDenial(ctx, in)
	if deny != nil {
		return *deny
	}
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

	// a direct ask run's prompt changes only by a Run: line fix; its recall rides additionalContext
	direct := opensWithAsk(task)
	session := func() string { u, _ := transcript.LastUsage(in.TranscriptPath); return u.Model }
	e := &effortPlan{updated: updated, run: newDispatchRun(session), mode: mode}
	task = e.fixRunLine(promptField, task)
	if deny := preflightBlock(ctx, e.run, task); deny != nil {
		return *deny
	}
	e.retype(in, fixType)
	return e.finish(ctx, in, promptField, task, direct)
}

// finish runs the recall, model pin and label steps and renders the result.
func (e *effortPlan) finish(ctx context.Context, in *core.Input, promptField, task string, direct bool) core.Result {
	st := in.Nested("tool_input", "subagent_type")
	extraNote := core.JoinContext(e.recallStep(ctx, in, promptField, task, true), e.benchNote(in.CWD, task))
	if e.mode == modeDeferred {
		return core.Result{Context: extraNote}
	}
	if !direct && !fixedModelTypes[st] {
		pinNote, pinMsg := pinModel(e.run, in, e.updated, task)
		e.fix(pinNote)
		e.systemMessage = strings.TrimSpace(e.systemMessage + " " + pinMsg)
		e.changed = e.changed || pinNote != ""
	}
	if !fixedModelTypes[st] {
		e.label(ctx, promptField, task)
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

func (e *effortPlan) retyped(st string) bool {
	return strings.HasPrefix(st, "coworker:") && e.updated["subagent_type"] != st
}

// effortDenial is the call's type or Run: denial, else the coworker type to retype to ("" for none).
func effortDenial(ctx context.Context, in *core.Input) (fixType string, deny *core.Result) {
	st, prompt := in.Nested("tool_input", "subagent_type"), in.Nested("tool_input", "prompt")
	if deny = runLineBlock(ctx, st, prompt); deny != nil {
		return "", deny
	}
	if deny = typeBlock(st, prompt); deny != nil {
		if fixType = coworkerTarget(st, prompt); fixType == "" {
			return "", deny
		}
		return fixType, nil
	}
	return "", nil
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

// recallStep is recall for the run's mode: a spawn mod's marker moves it to a note, its query skips it (it stamps).
func (e *effortPlan) recallStep(ctx context.Context, in *core.Input, promptField, task string, asNote bool) string {
	if e.mode == modeQuery {
		return ""
	}
	return e.recall(ctx, in, promptField, task, asNote)
}

// recall appends the memory-recall block to the prompt, or returns it as a note (direct ask run or auto mode).
func (e *effortPlan) recall(ctx context.Context, in *core.Input, promptField, task string, asNote bool) string {
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
func (e *effortPlan) label(ctx context.Context, field, task string) {
	if frozen, note := e.freezeAuto(ctx, task); frozen != task {
		e.updated[field], e.changed, task = frozen, true, frozen
		e.fix(note)
	}
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
