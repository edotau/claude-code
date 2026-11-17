// `dispatch-effort`: PreToolUse on spawn/Workflow: type deny, model pin, recall, label; fails OPEN.
package hooks

import (
	"context"
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
	if in.ToolName == "Workflow" {
		return workflows.WorkflowResult(in)
	}
	if !core.SpawnTools[in.ToolName] {
		return core.Result{}
	}
	if deny := runLineBlock(ctx, in.Nested("tool_input", "subagent_type"), in.Nested("tool_input", "prompt")); deny != nil {
		return *deny
	}
	fixType := ""
	if deny := typeBlock(in.Nested("tool_input", "subagent_type"), in.Nested("tool_input", "prompt")); deny != nil {
		if fixType = coworkerTarget(in.Nested("tool_input", "subagent_type"), in.Nested("tool_input", "prompt")); fixType == "" {
			return *deny
		}
	}
	// updatedInput REPLACES the whole tool_input record, so every original key must be echoed back.
	updated := in.CloneToolInput()
	if updated == nil {
		return core.Result{}
	}

	promptField, task := spawnTask(in)

	// a direct ask run's prompt stays byte-identical: its recall rides additionalContext
	direct := opensWithAsk(task)
	session := func() string { u, _ := transcript.LastUsage(in.TranscriptPath); return u.Model }
	e := &effortPlan{updated: updated, run: newDispatchRun(session)}
	if deny := preflightBlock(ctx, e.run, task); deny != nil {
		return *deny
	}
	e.retype(in, fixType)
	// auto mode refuses a hook-modified input, so its recall rides additionalContext too
	recallNote := e.recall(ctx, in, promptField, task, direct || in.Nested("permission_mode") == "auto")
	if !direct {
		pinNote, pinMsg := pinModel(e.run, in, updated, task)
		e.fix(pinNote)
		e.systemMessage = strings.TrimSpace(e.systemMessage + " " + pinMsg)
		e.changed = e.changed || pinNote != ""
	}
	e.label(ctx, task)
	e.note = strings.TrimSpace(e.note + "\n" + recallNote)
	return e.result()
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
}

// recall appends the memory-recall block to the prompt, or returns it as a note (direct ask run or auto mode).
func (e *effortPlan) recall(ctx context.Context, in *core.Input, promptField, task string, asNote bool) string {
	recallBlock, _ := memory.AgentRecall(ctx, in.SessionID, task)
	if recallBlock == "" {
		return ""
	}
	if asNote {
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
