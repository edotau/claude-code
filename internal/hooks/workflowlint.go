// `workflow-lint` — PreToolUse on Workflow: an inline script's unpinned agent() leaves are pinned to
// sonnet; ambiguous ones block.
package hooks

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/review/plan"
)

const workflowLintName = "inline workflow"

// WorkflowLint rewrites every unpinned agent() leaf in an inline script to pin model:'sonnet', or
// blocks when a leaf's options argument can't be rewritten safely or a pin's literal value is bad. A
// saved script given by scriptPath can't be rewritten, so it only blocks (on a model/agentType/value
// problem, never a missing effort — that's a `workflow lint` concern, not this hook's).
func WorkflowLint(r io.Reader, stdout, errw io.Writer) int {
	in := ParseInput(r)
	if in.ToolName != "Workflow" {
		return ExitProceed
	}
	if script := in.nested("tool_input", "script"); script != "" {
		return workflowLintInline(in, script, stdout, errw)
	}
	if path := in.nested("tool_input", "scriptPath"); path != "" {
		return workflowLintSaved(path, errw)
	}
	return ExitProceed // saved by name — nothing to lint
}

func workflowLintInline(in *Input, script string, stdout, errw io.Writer) int {
	out, amb := plan.FillModel(script, "sonnet")
	vals := badPinValueHits(plan.LintPins(workflowLintName, []byte(out)))
	if len(amb) > 0 || len(vals) > 0 {
		fmt.Fprintf(errw, "workflow-lint: blocked — an unpinned leaf runs on the session model (settings"+
			" \"model\"); pin model:'sonnet'|'haiku' explicitly:\n")
		for _, a := range amb {
			fmt.Fprintf(errw, "  ambiguous agent() options argument at %s\n", a)
		}
		for _, v := range vals {
			fmt.Fprintf(errw, "  %s\n", v)
		}
		return ExitBlock
	}
	if out == script {
		return ExitProceed
	}
	filled := filledModelLines(script, amb)
	updated := agentToolInput(in)
	if updated == nil {
		return ExitProceed
	}
	updated["script"] = out
	note := fmt.Sprintf("workflow-lint: filled model:'sonnet' on %d unpinned agent() call(s) at line(s)"+
		" %s; pin model explicitly to override", len(filled), strings.Join(filled, ", "))
	inner := map[string]any{"hookEventName": "PreToolUse", "additionalContext": note, "updatedInput": updated}
	enc, err := json.Marshal(map[string]any{"hookSpecificOutput": inner})
	if err != nil {
		return ExitProceed
	}
	fmt.Fprintln(stdout, string(enc))
	return ExitProceed
}

func workflowLintSaved(path string, errw io.Writer) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return ExitProceed // fail open — the hook cannot rewrite a file anyway
	}
	hits := withoutEffortOnly(plan.LintPins(path, data))
	if len(hits) == 0 {
		return ExitProceed
	}
	fmt.Fprintf(errw, "workflow-lint: blocked — %d agent() pin problem(s) in %s; edit the file and re-run:\n%s\n",
		len(hits), path, strings.Join(hits, "\n"))
	return ExitBlock
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
		if !strings.Contains(m, "without model:/agentType:") {
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
