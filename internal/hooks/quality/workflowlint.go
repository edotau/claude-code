// `dispatch-effort` — PreToolUse on Workflow: an inline script's unpinned agent() leaves are pinned to
// the medium-tier model; ambiguous ones block; a by-name call is redirected to its on-disk script and linted there.
package quality

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/router"
)

const workflowLintName = "inline workflow"

// WorkflowResult rewrites every unpinned agent() leaf in an inline script to pin the medium-tier model, or
// blocks when a leaf's options argument can't be rewritten safely or a pin's literal value is bad. A
// saved script given by scriptPath can't be rewritten, so it only blocks (on a model/agentType/value
// problem, never a missing effort — that's a `workflow lint` concern, not this hook's). A by-name call
// is redirected to the on-disk file's text as an inline script (the by-name registry is read once at
// session start, so an edit made mid-session would otherwise run the stale copy; a scriptPath outside
// the session's cwd is refused by the tool) and linted the same way; a name with no matching file (a
// built-in, or a bogus name) proceeds untouched.
func WorkflowResult(in *core.Input) core.Result {
	args := in.ToolInput()["args"]
	if script := in.Nested("tool_input", "script"); script != "" {
		if b := blockSteps("the inline script", checkWorkflowSteps([]byte(script), workflowsDir(), args)); b != nil {
			return *b
		}
		return workflowInline(in, script)
	}
	if path := paths.ExpandHome(in.Nested("tool_input", "scriptPath")); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return core.Result{} // fail open: the tool reports an unreadable scriptPath itself
		}
		if b := blockSteps(path, checkWorkflowSteps(data, filepath.Dir(path), args)); b != nil {
			return *b
		}
		return workflowSaved(path, data)
	}
	if name := in.Nested("tool_input", "name"); name != "" {
		return workflowByName(in, name)
	}
	return core.Result{}
}

func workflowInline(in *core.Input, script string) core.Result {
	model := models.AliasModel(models.EffortMedium)
	out, amb := FillModel(script, model)
	vals := badPinValueHits(LintPins(workflowLintName, []byte(out)))
	if len(amb) > 0 || len(vals) > 0 {
		var b strings.Builder
		b.WriteString("dispatch-effort: blocked — an unpinned leaf runs on the session model (settings" +
			" \"model\"); pin model:'sonnet'|'haiku' explicitly:")
		for _, a := range amb {
			fmt.Fprintf(&b, "\n  ambiguous agent() options argument at %s", a)
		}
		for _, v := range vals {
			fmt.Fprintf(&b, "\n  %s", v)
		}
		return core.Result{Decision: core.Block, Reason: b.String()}
	}
	if out == script {
		return core.Result{}
	}
	filled := filledModelLines(script, amb)
	updated := in.CloneToolInput()
	if updated == nil {
		return core.Result{}
	}
	updated["script"] = out
	note := fmt.Sprintf("dispatch-effort: filled model:'%s' on %d unpinned agent() call(s) at line(s)"+
		" %s; pin model explicitly to override", model, len(filled), strings.Join(filled, ", "))
	return core.Result{UpdatedInput: updated, Context: note}
}

func workflowSaved(path string, data []byte) core.Result {
	hits := lintSaved(path, data)
	if len(hits) == 0 {
		return core.Result{}
	}
	return blockSavedLint(path, hits)
}

// workflowByName redirects a saved-by-name call to its on-disk script's text via updatedInput.script,
// then lints that file exactly as a direct scriptPath call would. A path-shaped name, an empty config
// dir, or no matching file on disk all fail open, untouched.
func workflowByName(in *core.Input, name string) core.Result {
	if filepath.Base(name) != name {
		return core.Result{} // path-shaped, not a plain registry name — refuse to touch the dispatch
	}
	dir := paths.ConfigDir()
	if dir == "" {
		return core.Result{}
	}
	path := filepath.Join(dir, "workflows", name+".js")
	data, err := os.ReadFile(path) // read once: the lint, step check and rewrite all see these bytes
	if err != nil {
		return core.Result{} // no readable on-disk script for this name — built-in or bogus, pass through
	}
	if hits := lintSaved(path, data); len(hits) > 0 {
		return blockSavedLint(path, hits)
	}
	if b := blockSteps(path, checkWorkflowSteps(data, filepath.Dir(path), in.ToolInput()["args"])); b != nil {
		return *b
	}
	if _, underCWD := core.RelWithin(in.CWD, path); underCWD && in.Nested("permission_mode") == "auto" {
		return core.Result{Decision: core.Block, Reason: fmt.Sprintf("dispatch-effort: auto mode can't review a rewritten call; "+
			"re-issue as Workflow({scriptPath: %q, args: <same args>})", path)}
	}
	updated := in.CloneToolInput()
	if updated == nil {
		return core.Result{}
	}
	delete(updated, "name")
	updated["script"] = string(data) // inline, not scriptPath: the tool refuses a path outside the session's cwd
	note := fmt.Sprintf("dispatch-effort: name → inline script from %s (the by-name registry is read at session"+
		" start; the file on disk is current)", path)
	return core.Result{UpdatedInput: updated, Context: note}
}

// workflowsDir is where saved workflow scripts live ("" when the config dir is unknown).
func workflowsDir() string {
	if dir := paths.ConfigDir(); dir != "" {
		return filepath.Join(dir, "workflows")
	}
	return ""
}

// lintSaved returns the pin problems worth blocking on in a saved script's bytes.
func lintSaved(path string, data []byte) []string {
	return withoutEffortOnly(LintPins(path, data))
}

// blockSavedLint is the saved-script block naming path and its pin problems.
func blockSavedLint(path string, hits []string) core.Result {
	return core.Result{Decision: core.Block, Reason: fmt.Sprintf(
		"dispatch-effort: blocked — %d agent() pin problem(s) in %s; edit the file and re-run:\n%s",
		len(hits), path, strings.Join(hits, "\n"))}
}

// badPinValueHits keeps only the bad-literal-value messages from a LintPins result.
func badPinValueHits(msgs []string) []string {
	var out []string
	for _, m := range msgs {
		if strings.Contains(m, "is not one of") {
			out = append(out, m)
		}
	}
	return out
}

// withoutEffortOnly drops the missing-effort messages — a saved script can't be rewritten, so only a
// missing model/agentType or a bad pin value is worth blocking on here.
func withoutEffortOnly(msgs []string) []string {
	var out []string
	for _, m := range msgs {
		if !strings.Contains(m, "without effort:") {
			out = append(out, m)
		}
	}
	return out
}

// filledModelLines is the line numbers FillModel actually rewrote: every missing-model/agentType hit
// in the original script, minus the ones FillModel left ambiguous.
func filledModelLines(script string, ambiguous []string) []string {
	skip := make(map[string]bool, len(ambiguous))
	for _, a := range ambiguous {
		skip[strings.TrimPrefix(a, "line ")] = true
	}
	var lines []string
	for _, m := range LintPins(workflowLintName, []byte(script)) {
		if !strings.Contains(m, "without model:") {
			continue
		}
		rest := strings.TrimPrefix(m, workflowLintName+":")
		line := rest[:strings.IndexByte(rest, ':')]
		if !skip[line] {
			lines = append(lines, line)
		}
	}
	return lines
}

// The values stepOf accepts; the scripts keep only an unknown-stage/field backstop.
var (
	stepClaudeModels = []string{"haiku", "sonnet", "opus", "fable"}
	stepAgents       = agentid.All()
	stepEfforts      = agentid.EffortNames()
	stepFields       = []string{"agent", "model", "effort", "profile", "provider"}
	stepNameRe       = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[A-Za-z0-9._-]+$`) })
	// stepStageRe finds a script's own stages: stepOf(a.steps|OWN, '<stage>', …); a child's stages are not its own.
	stepStageRe = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`stepOf\(\s*(?:a\.steps|OWN)\s*,\s*'([A-Za-z0-9_-]+)'`)
	})
	// stepChildRe marks a script whose steps.child is a routed child's own steps (code-engineer → code-workers).
	stepChildRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`\{\s*child:\s*CHILD_RAW\b`) })
)

// stepChildScript is the workflow whose stages steps.child names.
const stepChildScript = "code-workers.js"

// stepRoute previews an external stage's route; a test swaps it (production reads the registry and route state).
var stepRoute = func(who, model, profile, provider string) error {
	chain, res, err := agent.Resolve(context.Background(), agent.RouteReq{Agent: who, Model: model, Profile: profile,
		Provider: provider, Fallback: router.FallbackAgents, Preview: true})
	if err != nil {
		return err
	}
	if len(chain.Dispatches) == 0 && len(res.Candidates) == 0 {
		return fmt.Errorf("no candidate passes the gates (see `claude-code gateway --agent %s`)", who)
	}
	return nil
}

// checkWorkflowSteps lists every problem in args for script src (child scripts are read from dir); nil = may run.
func checkWorkflowSteps(src []byte, dir string, args any) []string {
	stages := scriptStages(string(src))
	if len(stages) == 0 {
		return nil // a stepOf-free script has no steps contract
	}
	a, ok := decodeArgs(args)
	if !ok && args != nil && args != "" {
		return []string{"args must be a JSON object (or a JSON-encoded object string)"}
	}
	run := stepRun{profile: plainString(a["profile"]), provider: plainString(a["provider"])}
	if stepChildRe().Match(src) {
		if child, err := os.ReadFile(filepath.Join(dir, stepChildScript)); err == nil {
			run.child = scriptStages(string(child))
			stages = append(stages, "child")
		}
	}
	return run.check("steps", a["steps"], stages)
}

// blockSteps is the Block naming every steps problem, or nil when there is none.
func blockSteps(what string, problems []string) *core.Result {
	if len(problems) == 0 {
		return nil
	}
	return &core.Result{Decision: core.Block, Reason: fmt.Sprintf("dispatch-effort: blocked — %d problem(s) in %s's args; "+
		"fix them and re-run:\n  %s", len(problems), what, strings.Join(problems, "\n  "))}
}

// scriptStages is the sorted, de-duplicated stage names a script passes to stepOf for its own steps.
func scriptStages(src string) []string {
	var out []string
	for _, m := range stepStageRe().FindAllStringSubmatch(src, -1) {
		if !slices.Contains(out, m[1]) {
			out = append(out, m[1])
		}
	}
	slices.Sort(out)
	return out
}

// decodeArgs accepts args as an object or a JSON-encoded object string.
func decodeArgs(args any) (map[string]any, bool) {
	if s, ok := args.(string); ok {
		var m map[string]any
		if json.Unmarshal([]byte(s), &m) != nil {
			return nil, false
		}
		return m, true
	}
	m, ok := args.(map[string]any)
	return m, ok
}

func plainString(v any) string { s, _ := v.(string); return s }

// stepRun carries the run-wide profile/provider and the child's stages while one steps object is checked.
type stepRun struct {
	profile, provider string
	child             []string
}

// check validates one steps object against stages; "child" recurses into the child workflow's stages.
func (r stepRun) check(where string, steps any, stages []string) []string {
	if steps == nil {
		return nil
	}
	m, ok := steps.(map[string]any)
	if !ok {
		return []string{where + " must be an object"}
	}
	var out []string
	for _, k := range slices.Sorted(maps.Keys(m)) {
		switch {
		case !slices.Contains(stages, k):
			out = append(out, fmt.Sprintf("%s.%s: unknown stage (allowed: %s)", where, k, strings.Join(stages, "|")))
		case k == "child" && r.child != nil:
			out = append(out, r.check(where+".child", m[k], r.child)...)
		default:
			out = append(out, r.stage(where+"."+k, m[k])...)
		}
	}
	return out
}

// stage validates one stage's fields, then previews an external stage's route.
func (r stepRun) stage(where string, v any) []string {
	m, ok := v.(map[string]any)
	if !ok {
		return []string{where + " must be an object"}
	}
	out := fieldProblems(where, m)
	if len(out) > 0 {
		return out
	}
	who := plainString(m["agent"])
	if who == "" || who == "claude" {
		return claudeStageProblems(where, m)
	}
	profile, provider := cmp.Or(plainString(m["profile"]), r.profile), cmp.Or(plainString(m["provider"]), r.provider)
	if err := stepRoute(who, plainString(m["model"]), profile, provider); err != nil {
		return []string{fmt.Sprintf("%s: %s has no route: %v", where, who, err)}
	}
	return nil
}

// fieldProblems checks field names and value shapes common to every stage.
func fieldProblems(where string, m map[string]any) []string {
	var out []string
	for _, k := range slices.Sorted(maps.Keys(m)) {
		if p := fieldProblem(k, m[k]); p != "" {
			out = append(out, fmt.Sprintf("%s.%s%s", where, k, p))
		}
	}
	return out
}

// fieldProblem is the suffix describing what is wrong with one field, "" when it is fine.
func fieldProblem(k string, v any) string {
	s, isStr := v.(string)
	switch {
	case !slices.Contains(stepFields, k):
		return ": unknown field (allowed: " + strings.Join(stepFields, "|") + ")"
	case !isStr:
		return " must be a string"
	case k == "agent" && !slices.Contains(stepAgents, s):
		return " must be " + strings.Join(stepAgents, "|")
	case k == "effort" && !slices.Contains(stepEfforts, s):
		return " must be " + strings.Join(stepEfforts, "|")
	case k != "agent" && k != "effort" && !stepNameRe().MatchString(s):
		return " must be a plain name (no provider: prefix)"
	}
	return ""
}

// claudeStageProblems: an in-context claude stage runs on the session profile with a Claude alias.
func claudeStageProblems(where string, m map[string]any) []string {
	if m["profile"] != nil || m["provider"] != nil {
		return []string{where + ": claude stages run in-context on the session profile; " +
			"profile/provider need agent codex|copilot|gemini|opencode"}
	}
	if model := plainString(m["model"]); model != "" && !slices.Contains(stepClaudeModels, model) {
		return []string{fmt.Sprintf("%s.model must be %s", where, strings.Join(stepClaudeModels, "|"))}
	}
	return nil
}
