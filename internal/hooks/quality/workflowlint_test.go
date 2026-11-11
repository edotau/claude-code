package quality

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
)

// wfLintPayload builds the PreToolUse Workflow stdin JSON for an inline script or a saved one.
func wfLintPayload(t *testing.T, extra map[string]any) string {
	t.Helper()
	toolInput := map[string]any{}
	maps.Copy(toolInput, extra)
	b, err := json.Marshal(map[string]any{"tool_name": "Workflow", "tool_input": toolInput})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// runWorkflowLint runs WorkflowResult and renders it like the PreToolUse emitter: a block reason on stderr, else
// an updatedInput JSON line on stdout.
func runWorkflowLint(stdin string) (code int, stdout, stderr string) {
	res := WorkflowResult(core.ParseInput(strings.NewReader(stdin)))
	if res.Decision == core.Block {
		return core.ExitBlock, "", res.Reason + "\n"
	}
	if res.UpdatedInput == nil {
		return core.ExitProceed, "", ""
	}
	inner := map[string]any{"hookEventName": "PreToolUse", "updatedInput": res.UpdatedInput}
	if res.Context != "" {
		inner["additionalContext"] = res.Context
	}
	out, _ := json.Marshal(map[string]any{"hookSpecificOutput": inner})
	return core.ExitProceed, string(out) + "\n", ""
}

func TestWorkflowLintNonWorkflowToolProceeds(t *testing.T) {
	code, out, errb := runWorkflowLint(`{"tool_name":"Agent","tool_input":{}}`)
	if code != core.ExitProceed {
		t.Fatalf("exit = %d, want ExitProceed", code)
	}
	if out != "" || errb != "" {
		t.Fatalf("stdout=%q stderr=%q, want both empty", out, errb)
	}
}

func TestWorkflowLintInlineUnpinnedFillsModel(t *testing.T) {
	script := "// comment\n// comment2\nagent('x', { label: 'l' });\n"
	payload := wfLintPayload(t, map[string]any{"script": script, "args": map[string]any{"k": 1}})
	code, out, errb := runWorkflowLint(payload)
	if code != core.ExitProceed {
		t.Fatalf("exit = %d, want ExitProceed (stderr=%q)", code, errb)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("stdout = %q, want exactly one JSON line", out)
	}
	var doc struct {
		HSO struct {
			AdditionalContext string         `json:"additionalContext"`
			UpdatedInput      map[string]any `json:"updatedInput"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &doc); err != nil {
		t.Fatalf("invalid JSON (%v): %q", err, lines[0])
	}
	got, _ := doc.HSO.UpdatedInput["script"].(string)
	if !strings.Contains(got, "model: 'sonnet'") {
		t.Errorf("updatedInput.script = %q, want it to contain model: 'sonnet'", got)
	}
	if !strings.Contains(got, "label: 'l'") {
		t.Errorf("updatedInput.script = %q, want label: 'l' preserved", got)
	}
	if !strings.Contains(doc.HSO.AdditionalContext, "line(s) 3") {
		t.Errorf("additionalContext = %q, want it to mention line(s) 3", doc.HSO.AdditionalContext)
	}
	args, ok := doc.HSO.UpdatedInput["args"].(map[string]any)
	if !ok || args["k"] != float64(1) {
		t.Errorf("updatedInput.args = %v, want {k:1} echoed back", doc.HSO.UpdatedInput["args"])
	}
}

func TestWorkflowLintInlineFullyPinnedProceedsSilently(t *testing.T) {
	script := "agent('x', { model: 'sonnet' });\n"
	payload := wfLintPayload(t, map[string]any{"script": script})
	code, out, errb := runWorkflowLint(payload)
	if code != core.ExitProceed {
		t.Fatalf("exit = %d, want ExitProceed", code)
	}
	if out != "" || errb != "" {
		t.Fatalf("stdout=%q stderr=%q, want both empty for an already-pinned script", out, errb)
	}
}

func TestWorkflowLintInlineAmbiguousOptionsBlocks(t *testing.T) {
	script := "const opts = { model: 'sonnet' };\nagent('x', opts);\n"
	payload := wfLintPayload(t, map[string]any{"script": script})
	code, _, errb := runWorkflowLint(payload)
	if code != core.ExitBlock {
		t.Fatalf("exit = %d, want ExitBlock (stderr=%q)", code, errb)
	}
	if !strings.Contains(errb, "ambiguous") || !strings.Contains(errb, "line") {
		t.Errorf("stderr = %q, want it to contain %q and %q", errb, "ambiguous", "line")
	}
}

func TestWorkflowLintInlineBadModelValueBlocks(t *testing.T) {
	script := "agent('x', { model: 'gpt-5' });\n"
	payload := wfLintPayload(t, map[string]any{"script": script})
	code, _, errb := runWorkflowLint(payload)
	if code != core.ExitBlock {
		t.Fatalf("exit = %d, want ExitBlock (stderr=%q)", code, errb)
	}
	if !strings.Contains(errb, "gpt-5") {
		t.Errorf("stderr = %q, want it to contain %q", errb, "gpt-5")
	}
}

// Regression: "agent()" inside a string literal (the meta description) must not be mistaken for a
// call — a live bug had the masked scan miscount and flag an otherwise fully pinned script.
func TestWorkflowLintInlineStringMentionNotMiscounted(t *testing.T) {
	script := "export const meta = { description: 'calls agent() with pins' };\n" +
		"agent('x', { model: 'sonnet' });\n"
	payload := wfLintPayload(t, map[string]any{"script": script})
	code, out, errb := runWorkflowLint(payload)
	if code != core.ExitProceed {
		t.Fatalf("exit = %d, want ExitProceed (stderr=%q)", code, errb)
	}
	if out != "" {
		t.Errorf("stdout = %q, want empty", out)
	}
}

func TestWorkflowLintSavedUnpinnedBlocksAndNamesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "saved.js")
	if err := os.WriteFile(path, []byte("agent('x');\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	payload := wfLintPayload(t, map[string]any{"scriptPath": path})
	code, out, errb := runWorkflowLint(payload)
	if code != core.ExitBlock {
		t.Fatalf("exit = %d, want ExitBlock (stderr=%q)", code, errb)
	}
	if out != "" {
		t.Errorf("stdout = %q, want empty — a saved script is never rewritten", out)
	}
	if !strings.Contains(errb, path) {
		t.Errorf("stderr = %q, want it to name %q", errb, path)
	}
}

// A scriptPath under skills/*/scripts/ is linted exactly like one under workflows/ — the hook's
// path resolution does not care which directory the saved script lives in.
func TestWorkflowLintSavedUnderSkillsScriptsBlocksAndNamesFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "skills", "problem-solving", "scripts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "code-evaluator.js")
	if err := os.WriteFile(path, []byte("export const meta = { name: 'code-evaluator' }\nagent('x');\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	payload := wfLintPayload(t, map[string]any{"scriptPath": path})
	code, out, errb := runWorkflowLint(payload)
	if code != core.ExitBlock {
		t.Fatalf("exit = %d, want ExitBlock (stderr=%q)", code, errb)
	}
	if out != "" {
		t.Errorf("stdout = %q, want empty — a saved script is never rewritten", out)
	}
	if !strings.Contains(errb, path) {
		t.Errorf("stderr = %q, want it to name %q", errb, path)
	}
}

func TestWorkflowLintSavedMissingEffortOnlyProceeds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "saved.js")
	if err := os.WriteFile(path, []byte("agent('x', { model: 'sonnet' });\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	payload := wfLintPayload(t, map[string]any{"scriptPath": path})
	code, _, errb := runWorkflowLint(payload)
	if code != core.ExitProceed {
		t.Fatalf("exit = %d, want ExitProceed — a missing effort: is a `workflow lint` concern, not this hook's (stderr=%q)", code, errb)
	}
}

func TestWorkflowLintSavedMissingOnDiskProceeds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.js")
	payload := wfLintPayload(t, map[string]any{"scriptPath": path})
	code, _, _ := runWorkflowLint(payload)
	if code != core.ExitProceed {
		t.Fatalf("exit = %d, want ExitProceed — fail open, the hook cannot rewrite a file anyway", code)
	}
}

func TestWorkflowLintByNameUnknownProceeds(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // empty config dir — gate-loop.js does not exist here
	payload := wfLintPayload(t, map[string]any{"name": "gate-loop", "args": map[string]any{}})
	code, out, errb := runWorkflowLint(payload)
	if code != core.ExitProceed {
		t.Fatalf("exit = %d, want ExitProceed", code)
	}
	if out != "" || errb != "" {
		t.Fatalf("stdout=%q stderr=%q, want both empty for an unknown by-name run", out, errb)
	}
}

// wfLintWriteGateLoop installs workflows/gate-loop.js in a fresh config dir and returns its path.
func wfLintWriteGateLoop(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	wfDir := filepath.Join(dir, "workflows")
	if err := os.MkdirAll(wfDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(wfDir, "gate-loop.js")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

type wfLintInlineDoc struct {
	HSO struct {
		AdditionalContext string         `json:"additionalContext"`
		UpdatedInput      map[string]any `json:"updatedInput"`
	} `json:"hookSpecificOutput"`
}

// decodeWfLintInline parses the single JSON line the by-name rewrite writes to stdout.
func decodeWfLintInline(t *testing.T, out string) wfLintInlineDoc {
	t.Helper()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 1 || lines[0] == "" {
		t.Fatalf("stdout = %q, want exactly one JSON line", out)
	}
	var doc wfLintInlineDoc
	if err := json.Unmarshal([]byte(lines[0]), &doc); err != nil {
		t.Fatalf("invalid JSON (%v): %q", err, lines[0])
	}
	return doc
}

func TestWorkflowLintByNameInlinesScript(t *testing.T) {
	path := wfLintWriteGateLoop(t, "agent('x', { model: 'sonnet' });\n")
	payload := wfLintPayload(t, map[string]any{"name": "gate-loop", "args": map[string]any{"k": 1}})
	code, out, errb := runWorkflowLint(payload)
	if code != core.ExitProceed {
		t.Fatalf("exit = %d, want ExitProceed (stderr=%q)", code, errb)
	}
	doc := decodeWfLintInline(t, out)
	gotScript, _ := doc.HSO.UpdatedInput["script"].(string)
	if gotScript != "agent('x', { model: 'sonnet' });\n" {
		t.Errorf("updatedInput.script = %q, want the file's text inlined", gotScript)
	}
	if _, ok := doc.HSO.UpdatedInput["scriptPath"]; ok {
		t.Errorf("updatedInput carries scriptPath, want the inline script only (the tool refuses out-of-cwd paths)")
	}
	if _, ok := doc.HSO.UpdatedInput["name"]; ok {
		t.Errorf("updatedInput still carries %q, want name removed", "name")
	}
	args, ok := doc.HSO.UpdatedInput["args"].(map[string]any)
	if !ok || args["k"] != float64(1) {
		t.Errorf("updatedInput.args = %v, want {k:1} echoed back", doc.HSO.UpdatedInput["args"])
	}
	if !strings.Contains(doc.HSO.AdditionalContext, "gate-loop") || !strings.Contains(doc.HSO.AdditionalContext, path) {
		t.Errorf("additionalContext = %q, want it to mention the name and resolved path", doc.HSO.AdditionalContext)
	}
}

func TestWorkflowLintByNameResolvedFileUnpinnedBlocksAndNamesFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	wfDir := filepath.Join(dir, "workflows")
	if err := os.MkdirAll(wfDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(wfDir, "gate-loop.js")
	if err := os.WriteFile(path, []byte("agent('x');\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	payload := wfLintPayload(t, map[string]any{"name": "gate-loop"})
	code, out, errb := runWorkflowLint(payload)
	if code != core.ExitBlock {
		t.Fatalf("exit = %d, want ExitBlock (stderr=%q)", code, errb)
	}
	if out != "" {
		t.Errorf("stdout = %q, want empty on block", out)
	}
	if !strings.Contains(errb, path) {
		t.Errorf("stderr = %q, want it to name %q", errb, path)
	}
}

// TestWorkflowLintAutoModeByNameUnderCwdBlocksWithScriptPath: auto mode can't review a rewritten call, so a
// by-name call whose script is under the cwd is blocked with the scriptPath re-issue instruction.
func TestWorkflowLintAutoModeByNameUnderCwdBlocksWithScriptPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	if err := os.MkdirAll(filepath.Join(dir, "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "workflows", "gate-loop.js")
	if err := os.WriteFile(path, []byte("agent('x', { model: 'sonnet' });\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	in := fmt.Sprintf(`{"tool_name":"Workflow","permission_mode":"auto","cwd":%q,"tool_input":{"name":"gate-loop"}}`, dir)
	res := WorkflowResult(core.ParseInput(strings.NewReader(in)))
	if res.Decision != core.Block || !strings.Contains(res.Reason, "scriptPath: "+fmt.Sprintf("%q", path)) {
		t.Errorf("result = %+v, want a Block naming scriptPath %s", res, path)
	}
}

// realScript reads a saved workflow from this repo's workflows/ (the hook's real input).
func realScript(t *testing.T, name string) ([]byte, string) {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "..", "workflows"))
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return src, dir
}

// stubRoute makes every external stage routable except agent "codex" on profile "nope".
func stubRoute(t *testing.T) {
	orig := stepRoute
	stepRoute = func(who, _, profile, _ string) error {
		if who == "codex" && profile == "nope" {
			return errors.New("profile nope is not in the registry")
		}
		return nil
	}
	t.Cleanup(func() { stepRoute = orig })
}

func TestCheckWorkflowSteps(t *testing.T) {
	stubRoute(t)
	cases := []struct {
		script string
		args   any
		want   []string // substrings, one per expected problem; empty = the call may run
	}{
		{"code-workers.js", map[string]any{"steps": map[string]any{"fix": map[string]any{"effort": "huge"}}}, []string{"steps.fix.effort must be"}},
		{"code-workers.js", map[string]any{"steps": map[string]any{"nope": map[string]any{}}}, []string{"steps.nope: unknown stage (allowed: dedupe|find|fix|gate|rederive|review|simplify|test)"}},
		{"code-workers.js", map[string]any{"steps": map[string]any{"find": map[string]any{"model": "gpt-5", "agent": "bard"}}}, []string{"steps.find.agent must be"}},
		{"code-workers.js", map[string]any{"steps": map[string]any{"find": map[string]any{"model": "gpt-5"}}}, []string{"steps.find.model must be haiku|sonnet|opus|fable"}},
		{"code-workers.js", map[string]any{"steps": map[string]any{"find": map[string]any{"profile": "openai:x", "agent": "codex"}}}, []string{"must be a plain name"}},
		{"code-workers.js", `{"steps":{"review":{"agent":"codex","profile":"nope"}}}`, []string{"steps.review: codex has no route: profile nope"}},
		{"code-workers.js", `{"steps":{"bogus":{}}`, []string{"args must be a JSON object"}},
		{"code-workers.js", map[string]any{"profile": "nope", "steps": map[string]any{"review": map[string]any{"agent": "codex"}}}, []string{"codex has no route"}},
		{"code-engineer.js", map[string]any{"steps": map[string]any{"child": map[string]any{"bogus": map[string]any{}}, "size": map[string]any{"profile": "qa"}}},
			[]string{"steps.child.bogus: unknown stage", "steps.size: claude stages run in-context"}},
		{"code-workers.js", map[string]any{"steps": map[string]any{"fix": map[string]any{"model": "opus", "effort": "high"}, "review": map[string]any{"agent": "codex"}}}, nil},
		{"code-engineer.js", map[string]any{"steps": map[string]any{"impl": map[string]any{"model": "opus"}, "child": map[string]any{"fix": map[string]any{"effort": "high"}}}}, nil},
		{"green-keeper-loop.js", nil, nil},
	}
	for _, c := range cases {
		src, dir := realScript(t, c.script)
		got := checkWorkflowSteps(src, dir, c.args)
		if len(got) != len(c.want) {
			t.Errorf("%s %v: got %d problem(s) %q, want %d %q", c.script, c.args, len(got), got, len(c.want), c.want)
			continue
		}
		for i, w := range c.want {
			if !strings.Contains(got[i], w) {
				t.Errorf("%s %v: problem %d = %q, want it to contain %q", c.script, c.args, i, got[i], w)
			}
		}
	}
}

// A bad agent or effort names the whole roster and the whole scale, in order.
func TestFieldProblemListsRosterAndScale(t *testing.T) {
	if got, want := fieldProblem("agent", "x"), " must be claude|codex|copilot|gemini|opencode"; got != want {
		t.Errorf("agent problem %q, want %q", got, want)
	}
	if got, want := fieldProblem("effort", "x"), " must be low|medium|high|xhigh|max"; got != want {
		t.Errorf("effort problem %q, want %q", got, want)
	}
}
