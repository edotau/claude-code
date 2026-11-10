// `workflow-lint` — PreToolUse on Workflow: an inline script's unpinned agent() leaves are pinned to
// the medium-tier model; ambiguous ones block; a by-name call is redirected to its on-disk script and linted there.
package quality

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/review/planlint"
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
	out, amb := planlint.FillModel(script, model)
	vals := badPinValueHits(planlint.LintPins(workflowLintName, []byte(out)))
	if len(amb) > 0 || len(vals) > 0 {
		var b strings.Builder
		b.WriteString("workflow-lint: blocked — an unpinned leaf runs on the session model (settings" +
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
	note := fmt.Sprintf("workflow-lint: filled model:'%s' on %d unpinned agent() call(s) at line(s)"+
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
		return core.Result{Decision: core.Block, Reason: fmt.Sprintf("workflow-lint: auto mode can't review a rewritten call; "+
			"re-issue as Workflow({scriptPath: %q, args: <same args>})", path)}
	}
	updated := in.CloneToolInput()
	if updated == nil {
		return core.Result{}
	}
	delete(updated, "name")
	updated["script"] = string(data) // inline, not scriptPath: the tool refuses a path outside the session's cwd
	note := fmt.Sprintf("workflow-lint: name → inline script from %s (the by-name registry is read at session"+
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
	return withoutEffortOnly(planlint.LintPins(path, data))
}

// blockSavedLint is the saved-script block naming path and its pin problems.
func blockSavedLint(path string, hits []string) core.Result {
	return core.Result{Decision: core.Block, Reason: fmt.Sprintf(
		"workflow-lint: blocked — %d agent() pin problem(s) in %s; edit the file and re-run:\n%s",
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
	for _, m := range planlint.LintPins(workflowLintName, []byte(script)) {
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
