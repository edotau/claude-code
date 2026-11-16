// `dispatch-effort`: PreToolUse on spawn/Workflow: type deny, model pin, recall, label; fails OPEN.
package hooks

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

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hooks/quality"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/providers"
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
		return quality.WorkflowResult(in)
	}
	if !core.SpawnTools[in.ToolName] {
		return core.Result{}
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

	// a direct ask run is not rewritten: its non-functional edits ride additionalContext
	direct := opensWithAsk(task)
	auto := direct
	session := func() string { u, _ := transcript.LastUsage(in.TranscriptPath); return u.Model }
	e := &effortPlan{updated: updated, run: newDispatchRun(session)}
	if deny := preflightBlock(ctx, e.run, task); deny != nil {
		return *deny
	}
	if deny := e.retype(in, fixType, auto); deny != nil {
		return *deny
	}
	recallNote := e.recall(ctx, in, promptField, task, auto)
	if !direct {
		var pinMsg string
		e.note, pinMsg = pinModel(e.run, in, updated, task)
		e.systemMessage = strings.TrimSpace(e.systemMessage + " " + pinMsg)
		e.changed = e.changed || e.note != ""
	}
	if deny := e.label(ctx, task, auto); deny != nil {
		return *deny
	}
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

// recall appends the memory-recall block to the prompt, or returns it as a note on a direct ask run.
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

func appendSeen(path string, ids []string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(strings.Join(ids, "\n") + "\n")
	return err
}

// label prefixes the description with the run's model: a rewrite, or on a direct ask run a one-time deny.
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
