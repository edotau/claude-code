package hooks

import (
	"bytes"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

// runWorkflowLint runs the hook and returns its exit code plus the captured stdout/stderr.
func runWorkflowLint(stdin string) (code int, stdout, stderr string) {
	var out, errb bytes.Buffer
	code = WorkflowLint(strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

func TestWorkflowLintNonWorkflowToolProceeds(t *testing.T) {
	code, out, errb := runWorkflowLint(`{"tool_name":"Agent","tool_input":{}}`)
	if code != ExitProceed {
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
	if code != ExitProceed {
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
	if code != ExitProceed {
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
	if code != ExitBlock {
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
	if code != ExitBlock {
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
	if code != ExitProceed {
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
	if code != ExitBlock {
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
	if code != ExitProceed {
		t.Fatalf("exit = %d, want ExitProceed — a missing effort: is a `workflow lint` concern, not this hook's (stderr=%q)", code, errb)
	}
}

func TestWorkflowLintSavedMissingOnDiskProceeds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.js")
	payload := wfLintPayload(t, map[string]any{"scriptPath": path})
	code, _, _ := runWorkflowLint(payload)
	if code != ExitProceed {
		t.Fatalf("exit = %d, want ExitProceed — fail open, the hook cannot rewrite a file anyway", code)
	}
}

func TestWorkflowLintByNameUnknownProceeds(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // empty config dir — gate-loop.js does not exist here
	payload := wfLintPayload(t, map[string]any{"name": "gate-loop", "args": map[string]any{}})
	code, out, errb := runWorkflowLint(payload)
	if code != ExitProceed {
		t.Fatalf("exit = %d, want ExitProceed", code)
	}
	if out != "" || errb != "" {
		t.Fatalf("stdout=%q stderr=%q, want both empty for an unknown by-name run", out, errb)
	}
}

func TestWorkflowLintByNameResolvesAndFillsScriptPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	wfDir := filepath.Join(dir, "workflows")
	if err := os.MkdirAll(wfDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(wfDir, "gate-loop.js")
	if err := os.WriteFile(path, []byte("agent('x', { model: 'sonnet' });\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	payload := wfLintPayload(t, map[string]any{"name": "gate-loop", "args": map[string]any{"k": 1}})
	code, out, errb := runWorkflowLint(payload)
	if code != ExitProceed {
		t.Fatalf("exit = %d, want ExitProceed (stderr=%q)", code, errb)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 1 || lines[0] == "" {
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
	gotPath, _ := doc.HSO.UpdatedInput["scriptPath"].(string)
	if gotPath != path {
		t.Errorf("updatedInput.scriptPath = %q, want %q", gotPath, path)
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
	if code != ExitBlock {
		t.Fatalf("exit = %d, want ExitBlock (stderr=%q)", code, errb)
	}
	if out != "" {
		t.Errorf("stdout = %q, want empty on block", out)
	}
	if !strings.Contains(errb, path) {
		t.Errorf("stderr = %q, want it to name %q", errb, path)
	}
}

// TestRegistryMatchesHookspec only asserts the name is present; dispatch itself is untested there.
func TestWorkflowLintRegistryDispatches(t *testing.T) {
	code := Run("workflow-lint", strings.NewReader(`{"tool_name":"Agent","tool_input":{}}`), &bytes.Buffer{}, &bytes.Buffer{})
	if code != ExitProceed {
		t.Fatalf("Run(\"workflow-lint\", ...) on an Agent payload = %d, want ExitProceed", code)
	}
}
