// `workflow-lint` — PreToolUse on Workflow: an inline script's unpinned agent() leaves are pinned to
// the medium-tier model; ambiguous ones block; a by-name call is redirected to its on-disk script and linted there.
package hooks

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/review/plan"
)

const workflowLintName = "inline workflow"

// WorkflowLint is the legacy `hook workflow-lint` entry: workflowResult rendered in the hook JSON shape.
func WorkflowLint(r io.Reader, stdout, errw io.Writer) int {
	in := ParseInput(r)
	if in.ToolName != "Workflow" {
		return ExitProceed
	}
	return renderEffort(workflowResult(in), stdout, errw)
}

// workflowResult rewrites every unpinned agent() leaf in an inline script to pin the medium-tier model, or
// blocks when a leaf's options argument can't be rewritten safely or a pin's literal value is bad. A
// saved script given by scriptPath can't be rewritten, so it only blocks (on a model/agentType/value
// problem, never a missing effort — that's a `workflow lint` concern, not this hook's). A by-name call
// is redirected to the on-disk file's text as an inline script (the by-name registry is read once at
// session start, so an edit made mid-session would otherwise run the stale copy; a scriptPath outside
// the session's cwd is refused by the tool) and linted the same way; a name with no matching file (a
// built-in, or a bogus name) proceeds untouched.
func workflowResult(in *Input) Result {
	if script := in.nested("tool_input", "script"); script != "" {
		return workflowInline(in, script)
	}
	if path := in.nested("tool_input", "scriptPath"); path != "" {
		return workflowSaved(path)
	}
	if name := in.nested("tool_input", "name"); name != "" {
		return workflowByName(in, name)
	}
	return Result{}
}

func workflowInline(in *Input, script string) Result {
	model := models.TierModel(models.EffortMedium)
	out, amb := plan.FillModel(script, model)
	vals := badPinValueHits(plan.LintPins(workflowLintName, []byte(out)))
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
		return Result{Decision: Block, Reason: b.String()}
	}
	if out == script {
		return Result{}
	}
	filled := filledModelLines(script, amb)
	updated := in.cloneToolInput()
	if updated == nil {
		return Result{}
	}
	updated["script"] = out
	note := fmt.Sprintf("workflow-lint: filled model:'%s' on %d unpinned agent() call(s) at line(s)"+
		" %s; pin model explicitly to override", model, len(filled), strings.Join(filled, ", "))
	return Result{UpdatedInput: updated, Context: note}
}

func workflowSaved(path string) Result {
	hits := lintSavedFile(path)
	if len(hits) == 0 {
		return Result{}
	}
	return blockSavedLint(path, hits)
}

// workflowByName redirects a saved-by-name call to its on-disk script's text via updatedInput.script,
// then lints that file exactly as a direct scriptPath call would. A path-shaped name, an empty config
// dir, or no matching file on disk all fail open, untouched.
func workflowByName(in *Input, name string) Result {
	if filepath.Base(name) != name {
		return Result{} // path-shaped, not a plain registry name — refuse to touch the dispatch
	}
	dir := paths.ConfigDir()
	if dir == "" {
		return Result{}
	}
	path := filepath.Join(dir, "workflows", name+".js")
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		return Result{} // no on-disk script for this name — built-in or bogus, pass through
	}
	if hits := lintSavedFile(path); len(hits) > 0 {
		return blockSavedLint(path, hits)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Result{}
	}
	updated := in.cloneToolInput()
	if updated == nil {
		return Result{}
	}
	delete(updated, "name")
	updated["script"] = string(data) // inline, not scriptPath: the tool refuses a path outside the session's cwd
	note := fmt.Sprintf("workflow-lint: name → inline script from %s (the by-name registry is read at session"+
		" start; the file on disk is current)", path)
	return Result{UpdatedInput: updated, Context: note}
}

// lintSavedFile reads path and returns the pin problems worth blocking on; a read error returns nil
// (fail open — the hook cannot rewrite a file anyway).
func lintSavedFile(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return withoutEffortOnly(plan.LintPins(path, data))
}

// blockSavedLint is the saved-script block naming path and its pin problems.
func blockSavedLint(path string, hits []string) Result {
	return Result{Decision: Block, Reason: fmt.Sprintf(
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
	for _, m := range plan.LintPins(workflowLintName, []byte(script)) {
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
