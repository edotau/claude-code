package quality

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
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

func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// agent() without effort: is flagged; a model/agentType pin does NOT satisfy it (effort is an
// independent routing axis), while opus-ok and word-prefixed calls are skipped.
func TestEffortlessAgentCalls(t *testing.T) {
	p := writeScript(t, t.TempDir(), "wf.js", `const a = await agent('task one', { label: 'x', effort: 'low' })
const b = await agent('task two', { label: 'y', model: 'sonnet' })
const c = await agent('judge', { label: 'j' }) // lint:opus-ok
const d = await subagent('not the primitive')
`)
	hits := effortlessAgentCalls(p, readScript(t, p))
	if len(hits) != 1 || hits[0] != p+":2" {
		t.Fatalf("hits = %v, want exactly the line-2 call (a model pin does not imply effort)", hits)
	}
}

// Unpinned agent() is flagged; only a model: pin satisfies it — agentType alone does not (a built-in
// Explore leaf ran opus under an agentType-only "pin"), and word-prefixed calls (subagent(), x.agent())
// are never the primitive. lint:opus-ok exempts ONLY the effort check (TestEffortlessAgentCalls).
func TestUnpinnedAgentCalls(t *testing.T) {
	p := writeScript(t, t.TempDir(), "wf.js", `const a = await agent('task one', { label: 'x', schema: S })
const b = await agent('task two', { label: 'y', model: 'sonnet' })
const c = await agent('task three', { agentType: 'claude' })
const d = await agent('judge', { label: 'j' }) // lint:opus-ok
const e = await subagent('not the primitive')
`)
	hits := unpinnedAgentCalls(p, readScript(t, p))
	if len(hits) != 3 || hits[0] != p+":1" || hits[1] != p+":3" || hits[2] != p+":4" {
		t.Fatalf("hits = %v, want lines 1, 3 and 4 (agentType alone and opus-ok do not pin a model)", hits)
	}
}

// A prompt string containing "effort:"/"model:" text must not satisfy the pin check, and a paren
// inside the prompt must not end the call window before the real options object.
func TestStringLiteralDoesNotMaskPin(t *testing.T) {
	p := writeScript(t, t.TempDir(), "wf.js", "const a = await agent('task', { prompt: \"remember effort: low later, model: sonnet too\" })\n"+
		"const b = await agent(`run f(x) then ) report`, { model: 'sonnet', effort: 'low' })\n")
	src := readScript(t, p)
	if hits := effortlessAgentCalls(p, src); len(hits) != 1 || hits[0] != p+":1" {
		t.Fatalf("effortlessAgentCalls = %v, want only line 1", hits)
	}
	if hits := unpinnedAgentCalls(p, src); len(hits) != 1 || hits[0] != p+":1" {
		t.Fatalf("unpinnedAgentCalls = %v, want only line 1", hits)
	}
}

// A read error must surface as a hit, not silently report the file clean.
func TestUnreadableFileIsAHit(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.js")
	if hits := registryHidingDefects(missing); len(hits) != 1 || !strings.Contains(hits[0], "cannot read file") {
		t.Errorf("registryHidingDefects(missing) = %v, want one 'cannot read file' hit", hits)
	}
}

// A non-literal meta and a top-level throw both make the saved-workflow registry skip a script silently;
// indented throws and clean metas pass.
func TestRegistryHidingDefects(t *testing.T) {
	dir := t.TempDir()
	clean := writeScript(t, dir, "clean.js", "export const meta = {\n  name: 'x',\n  description: 'a {b} c with a + sign and a $ inside prose',\n}\nconst a = args || {}\nif (!a.t) return { error: 'x' }\nfunction f() {\n  throw new Error('inside')\n}\n")
	if got := registryHidingDefects(clean); len(got) != 0 {
		t.Errorf("clean script flagged: %v", got)
	}
	concat := writeScript(t, dir, "concat.js", "export const meta = {\n  name: 'x',\n  whenToUse:\n    'a' +\n    'b',\n}\n")
	if got := registryHidingDefects(concat); len(got) != 1 || !strings.Contains(got[0], "pure literal") {
		t.Errorf("concatenated meta: got %v, want one pure-literal hit", got)
	}
	thrown := writeScript(t, dir, "throw.js", "export const meta = {\n  name: 'x',\n}\nconst a = args || {}\nif (!a.t) throw new Error('needs t')\nthrow new Error('bare')\n")
	got := registryHidingDefects(thrown)
	if len(got) != 2 || !strings.Contains(got[0], "throw.js:5") || !strings.Contains(got[1], "throw.js:6") {
		t.Errorf("top-level throws: got %v, want hits on lines 5 and 6", got)
	}
}

// An apostrophe in a comment must not open a string and blank the pin after it.
func TestStripStringsSkipsComments(t *testing.T) {
	got := stripStrings("agent('x', {\n  // don't inherit session effort\n  effort: 'high',\n  /* it's pinned */ model: 'sonnet' })")
	if !strings.Contains(got, "effort:") || !strings.Contains(got, "model:") {
		t.Fatalf("stripStrings blanked a real pin: %q", got)
	}
}

// The verb, for workflows/*.js and skills/*/scripts/*.js alike: clean exits 0 with the count, a flagged one exits 1.
func TestRunWorkflowLint(t *testing.T) {
	noNode(t)
	for _, rel := range []string{"workflows", filepath.Join("skills", "s", "scripts")} {
		t.Run(rel, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, rel)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			writeScript(t, dir, "ok.js", "export const meta = { name: 'ok' }\nawait agent('x', { model: 'sonnet', effort: 'low' })\n")
			var out, errb bytes.Buffer
			if rc := RunWorkflowLint([]string{root}, &out, &errb); rc != 0 || out.String() != "workflow lint: 1 script(s) clean\n" {
				t.Fatalf("clean: rc=%d out=%q err=%q", rc, out.String(), errb.String())
			}
			writeScript(t, dir, "bad.js", "export const meta = { name: 'bad' }\nawait agent('x', { label: 'l' })\n")
			out.Reset()
			errb.Reset()
			if rc := RunWorkflowLint([]string{root}, &out, &errb); rc != 1 || strings.Count(errb.String(), "bad.js:2") != 2 || out.Len() != 0 ||
				!strings.Contains(errb.String(), "agent() without model:") || !strings.Contains(errb.String(), "agent() without effort:") {
				t.Fatalf("flagged: rc=%d out=%q err=%q", rc, out.String(), errb.String())
			}
		})
	}
}

// A top-level throw hides the script from the registry: the verb reports it at its path and line and exits 1.
func TestRunWorkflowLintReportsTopLevelThrow(t *testing.T) {
	noNode(t)
	dir := filepath.Join(t.TempDir(), "workflows")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := writeScript(t, dir, "throws.js", "export const meta = { name: 'throws' }\nthrow new Error('boom')\n")
	var out, errb bytes.Buffer
	rc := RunWorkflowLint([]string{filepath.Dir(dir)}, &out, &errb)
	if rc == 0 || !strings.Contains(errb.String(), p+":2: top-level throw") || out.Len() != 0 {
		t.Fatalf("rc=%d out=%q err=%q; want non-zero and %s:2: top-level throw", rc, out.String(), errb.String(), p)
	}
}

// isWorkflowScript excludes a plain helper script under scripts/ — even one with an unpinned agent()
// call — and includes one whose first non-blank line is a // comment ahead of `export const meta`.
func TestIsWorkflowScript(t *testing.T) {
	dir := t.TempDir()
	helper := writeScript(t, dir, "helper.js", "// just a utility\nfunction run(opts) {\n  return agent('x', { label: 'l' })\n}\n")
	if isWorkflowScript(helper) {
		t.Errorf("isWorkflowScript(helper) = true, want false — no export const meta opener")
	}
	commented := writeScript(t, dir, "commented.js", "// Setup before running: do the thing.\n\nexport const meta = { name: 'x' }\n")
	if !isWorkflowScript(commented) {
		t.Errorf("isWorkflowScript(commented) = false, want true — a leading comment before meta still counts")
	}
}

// A root with no workflows/*.js is an error naming the searched path, not "0 script(s) clean".
func TestRunWorkflowLintNoScripts(t *testing.T) {
	root := t.TempDir()
	var out, errb bytes.Buffer
	want := filepath.Join(root, "workflows")
	if rc := RunWorkflowLint([]string{root}, &out, &errb); rc != 1 || !strings.Contains(errb.String(), want) || out.Len() != 0 {
		t.Fatalf("empty root: rc=%d out=%q err=%q; want 1, stderr naming %s", rc, out.String(), errb.String(), want)
	}
}

// badPinValues flags a literal pin value outside the allow-list; a ternary/identifier value can't be
// checked statically and passes, a string literal elsewhere containing "model:" text must not be
// read as a pin, and lint:opus-ok exempts the whole call (not just the effort axis, unlike LintPins).
func TestBadPinValues(t *testing.T) {
	p := writeScript(t, t.TempDir(), "wf.js", "agent('a', { model: 'gpt-5', effort: 'low' })\n"+
		"agent('b', { model: 'sonnet', effort: 'med' })\n"+
		"agent('c', { model: hot ? 'opus' : 'sonnet', effort: x.effort || 'low' })\n"+
		"agent('d', { model: s.model, effort: s.effort })\n"+
		"agent('e', { label: \"model: gpt-5\", model: 'haiku', effort: 'high' })\n"+
		"agent('f', { model: 'fable', effort: 'high' }) // lint:opus-ok\n")
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	hits := badPinValues(p, string(data))
	if len(hits) != 2 {
		t.Fatalf("hits = %v, want exactly 2 (line 1 gpt-5, line 2 med)", hits)
	}
	if !strings.Contains(hits[0], p+":1:") || !strings.Contains(hits[0], "model") || !strings.Contains(hits[0], `"gpt-5"`) {
		t.Errorf("hits[0] = %q, want line 1 naming key model and value gpt-5", hits[0])
	}
	if !strings.Contains(hits[1], p+":2:") || !strings.Contains(hits[1], "effort") || !strings.Contains(hits[1], `"med"`) {
		t.Errorf("hits[1] = %q, want line 2 naming key effort and value med", hits[1])
	}
}

// routeTier('<tier>') wraps the literal in a call (the router-pressure helper added to the saved
// workflows); it still counts as pinned, and the wrapped tier literal is still validated against the
// allow-list. routeTier(cfg.x) — no literal at all — is accepted today: badPinValues finds no quoted
// literal to check, so it takes no position; this test pins that current (lenient) behavior down.
func TestRouteTierWrappedModelPin(t *testing.T) {
	good := []byte("agent('a', { model: routeTier('sonnet'), effort: 'low' })\n")
	if hits := LintPins("inline", good); len(hits) != 0 {
		t.Fatalf("LintPins(routeTier('sonnet')) = %v, want none — the wrapped literal is a valid pin", hits)
	}
	bad := []byte("agent('a', { model: routeTier('gpt-5'), effort: 'low' })\n")
	if hits := LintPins("inline", bad); len(hits) != 1 || !strings.Contains(hits[0], `"gpt-5"`) {
		t.Fatalf("LintPins(routeTier('gpt-5')) = %v, want one hit naming value gpt-5", hits)
	}
	noLiteral := []byte("agent('a', { model: routeTier(cfg.x), effort: 'low' })\n")
	if hits := LintPins("inline", noLiteral); len(hits) != 0 {
		t.Fatalf("LintPins(routeTier(cfg.x)) = %v, want none — current behavior accepts an unchecked"+
			" identifier argument silently (no literal to validate, and model: text alone pins it)", hits)
	}
}

// D1: lint:opus-ok exempts only the effort axis — a marked call missing model: is still a model hit.
func TestOpusOKExemptsEffortOnly(t *testing.T) {
	hits := LintPins("inline", []byte("await agent('x', { label: 'l' }) // lint:opus-ok\n"))
	if len(hits) != 1 {
		t.Fatalf("hits = %v, want exactly 1 (model hit, no effort hit)", hits)
	}
	if !strings.Contains(hits[0], "without model:") {
		t.Errorf("hits[0] = %q, want the model-missing message", hits[0])
	}
	if strings.Contains(hits[0], "effort:") {
		t.Errorf("hits[0] = %q, must not also carry the effort-missing message", hits[0])
	}
}

// The shipped scripts must satisfy every check — a lint that lands red breaks make audit everywhere.
// Anchored on this source file's path so a cwd change can never make the guard skip instead of gate.
func TestShippedWorkflowsAreClean(t *testing.T) {
	useNode(t)
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed — cannot locate the repo root")
	}
	repoRoot := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))) // internal/hooks/quality/x_test.go -> repo
	var out, errb bytes.Buffer
	if rc := RunWorkflowLint([]string{repoRoot}, &out, &errb); rc != 0 || !strings.Contains(out.String(), "script(s) clean") || strings.HasPrefix(out.String(), "workflow lint: 0 ") {
		t.Fatalf("shipped workflows under %s: rc=%d out=%q\n%s", repoRoot, rc, out.String(), errb.String())
	}
}

func TestAgentCallWhitespaceAndIdentifierBoundary(t *testing.T) {
	for _, tc := range []struct {
		src      string
		want     string
		wantHits int
	}{
		{"agent ('p')", "agent ('p', { model: 'sonnet' })", 2},
		{"agent\t('p')", "agent\t('p', { model: 'sonnet' })", 2},
		{"agent\n('p')", "agent\n('p', { model: 'sonnet' })", 2},
		{"$agent('p')", "$agent('p')", 0},
		{"subagent('p')", "subagent('p')", 0},
		{"x.agent('p')", "x.agent('p')", 0},
		{"agentic ('p')", "agentic ('p')", 0},
	} {
		t.Run(tc.src, func(t *testing.T) {
			out, ambiguous := FillModel(tc.src, "sonnet")
			if out != tc.want || len(ambiguous) != 0 {
				t.Errorf("FillModel = %q, %v; want %q, no ambiguity", out, ambiguous, tc.want)
			}
			if hits := LintPins("inline", []byte(tc.src)); len(hits) != tc.wantHits {
				t.Errorf("LintPins = %v, want %d hits", hits, tc.wantHits)
			}
		})
	}
}

func readScript(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// An apostrophe in a comment inside agent({...}) must not open a string and swallow the close paren.
func TestCallWindowSkipsCommentApostrophe(t *testing.T) {
	src := "agent('x', { // don't wait\n model: 'sonnet' })\nagent('y', { model: 'opus' })"
	if got := delimitedWindow(src, stripStrings(src), 0, '(', ')'); strings.Contains(got, "opus") {
		t.Fatalf("window %q spilled into the next call", got)
	}
}

// A nested unescaped backtick (the b81c06ae shape) does not compile; the escaped fix (6006600a) does.
func TestWorkflowParseDefects(t *testing.T) {
	useNode(t)
	dir := t.TempDir()
	bad := writeScript(t, dir, "bad.js", "export const meta = {}\nconst p = `run `git show` now`\n")
	good := writeScript(t, dir, "good.js", "export const meta = {}\nconst p = `run \\`git show\\` now`\n")
	if hits, _ := workflowParseDefects([]string{good}); len(hits) != 0 {
		t.Fatalf("good script flagged: %v", hits)
	}
	hits, _ := workflowParseDefects([]string{bad, good})
	if len(hits) != 1 || !strings.Contains(hits[0], "bad.js") || !strings.Contains(hits[0], "does not parse") {
		t.Fatalf("want one bad.js parse hit, got %v", hits)
	}
}

func TestWorkflowParseSkippedWithoutNode(t *testing.T) {
	noNode(t)
	hits, skipped := workflowParseDefects([]string{"x.js"})
	if len(hits) != 0 || !strings.Contains(skipped, "parse check skipped") {
		t.Fatalf("hits=%v skipped=%q", hits, skipped)
	}
}

// One unreadable script is one defect: lintScript reports it, node is never handed it to re-report as a parse error.
func TestUnreadableScriptReportedOnce(t *testing.T) {
	root := t.TempDir()
	wf := filepath.Join(root, "workflows")
	if err := os.Mkdir(wf, 0o755); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		t.Skip("needs a sh stand-in for node")
	}
	if err := os.Symlink(filepath.Join(root, "gone.js"), filepath.Join(wf, "dangling.js")); err != nil {
		t.Skip("symlinks unavailable")
	}
	fake := writeScript(t, root, "fakenode", "#!/bin/sh\nshift 2\nfor f; do echo \"$f: ENOENT\"; done\nexit 1\n")
	if err := os.Chmod(fake, 0o755); err != nil {
		t.Fatal(err)
	}
	orig := lookNode
	lookNode = func() (string, error) { return fake, nil }
	t.Cleanup(func() { lookNode = orig })
	var out, errb bytes.Buffer
	if rc := RunWorkflowLint([]string{root}, &out, &errb); rc != 1 {
		t.Fatalf("rc = %d, want 1", rc)
	}
	if got := errb.String(); strings.Count(got, "cannot read file") != 1 || strings.Contains(got, "does not parse") {
		t.Fatalf("stderr = %q, want one 'cannot read file' and no 'does not parse'", got)
	}
}

// Only parseCheckJS's own "<file>: <message>" lines are parse failures; node warnings beside them are not relabelled.
func TestParseDefectsIgnoreNodeNoise(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a sh stand-in for node")
	}
	dir := t.TempDir()
	script := writeScript(t, dir, "a.js", "x\n")
	fake := writeScript(t, dir, "fakenode", "#!/bin/sh\necho '(node:1) Warning: noisy'\necho '"+script+": boom'\nexit 1\n")
	if err := os.Chmod(fake, 0o755); err != nil {
		t.Fatal(err)
	}
	orig := lookNode
	lookNode = func() (string, error) { return fake, nil }
	t.Cleanup(func() { lookNode = orig })
	hits, skipped := workflowParseDefects([]string{script})
	if skipped != "" || len(hits) != 1 || hits[0] != script+": boom (does not parse)" {
		t.Fatalf("hits = %q skipped = %q, want only the file's own line labelled", hits, skipped)
	}
}

// Every top-level `export` is stripped before the parse check, not only the first.
func TestParseCheckStripsEveryExport(t *testing.T) {
	useNode(t)
	p := writeScript(t, t.TempDir(), "two.js", "export const meta = {}\nexport const x = 1\n")
	if hits, _ := workflowParseDefects([]string{p}); len(hits) != 0 {
		t.Fatalf("two exports reported as parse defect: %v", hits)
	}
}

// A throw in a module-level block fires at import and is reported on its line; one in a callable body never fires.
func TestRegistryHidingTopLevelThrows(t *testing.T) {
	const fire, deferred = "throw new Error('import')", "throw new Error('deferred')"
	cases := []struct {
		name, body string
		want       []int
	}{
		{"if block", "if (!process.env.X) {\n  " + fire + "\n}\n", []int{3}},
		{"inline if block", "if (c) { " + fire + " }\n", []int{2}},
		{"bare block", "{\n  " + fire + "\n}\n", []int{3}},
		{"for await block", "for await (const item of items) {\n  " + fire + "\n}\n", []int{3}},
		{"block after a call", "initialize()\n{ " + fire + " }\n", []int{3}},
		{"nested block after a call", "{ initialize()\n{ " + fire + " } }\n", []int{3}},
		{"labeled block after a call", "label: { initialize()\n{ " + fire + " } }\n", []int{3}},
		{"object method", "const obj = { initialize() { " + deferred + " } }\n", nil},
		{"class method", "class Worker { initialize() { " + deferred + " } }\n", nil},
		{"object method then module if", "const obj = { run(x) { if (x) { " + deferred + " } } }\nif (ready()) { " + fire + " }\n", []int{3}},
		{"class method then module if", "class Worker { run(x) { if (x) { " + deferred + " } } }\nif (ready()) { " + fire + " }\n", []int{3}},
		{"arrow and function bodies", "const f = () => {\n  " + deferred + "\n}\nfunction g(a, b) {\n  if (a) { " + deferred + " }\n}\n", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := writeScript(t, t.TempDir(), "wf.js", "export const meta = { name: 'x' }\n"+tc.body)
			hits := registryHidingDefects(p)
			if len(hits) != len(tc.want) {
				t.Fatalf("registryHidingDefects = %v, want top-level throws on lines %v", hits, tc.want)
			}
			for i, line := range tc.want {
				if want := fmt.Sprintf("%s:%d: top-level throw", p, line); !strings.HasPrefix(hits[i], want) {
					t.Errorf("hit %d = %q, want prefix %q", i, hits[i], want)
				}
			}
		})
	}
}

// FillModel inserts the pin into every literal options shape it can edit and refuses the ones it cannot.
func TestFillModel(t *testing.T) {
	ambiguous := []string{"line 1"}
	cases := []struct {
		name, src, want string
		ambiguous       []string
	}{
		{"label-only object", "agent('p', { label: 'l' })\n", "agent('p', { model: 'sonnet', label: 'l' })\n", nil},
		{"empty object", "agent('p', {})\n", "agent('p', {model: 'sonnet'})\n", nil},
		{"one-arg call", "agent('p')\n", "agent('p', { model: 'sonnet' })\n", nil},
		{"two calls back to front", "agent('a', { label: 'x' })\nagent('b', { label: 'y' })\n",
			"agent('a', { model: 'sonnet', label: 'x' })\nagent('b', { model: 'sonnet', label: 'y' })\n", nil},
		{"template literal is not a call", "const prompt = `call agent(x) with model: sonnet`\nagent('p', { label: 'l' })\n",
			"const prompt = `call agent(x) with model: sonnet`\nagent('p', { model: 'sonnet', label: 'l' })\n", nil},
		{"already pinned", "agent('p', { model: 'opus' })\n", "agent('p', { model: 'opus' })\n", nil},
		{"pinned after agentType", "agent('p', { agentType: 'claude', model: 'sonnet' })\n",
			"agent('p', { agentType: 'claude', model: 'sonnet' })\n", nil},
		{"agent() inside a string", "export const meta = { description: 'an unpinned agent() probe' }\n",
			"export const meta = { description: 'an unpinned agent() probe' }\n", nil},
		{"regex literal", "const re = /agent(foo)/;", "const re = /agent(foo)/;", nil},
		{"bare identifier options", "agent('p', opts)\n", "agent('p', opts)\n", ambiguous},
		{"spread options", "agent('p', ...opts)\n", "agent('p', ...opts)\n", ambiguous},
		{"ternary options", "agent('p', cond ? a : b)\n", "agent('p', cond ? a : b)\n", ambiguous},
		{"object spread inside options", "agent('p', { ...leafOpts })\n", "agent('p', { ...leafOpts })\n", ambiguous},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, amb := FillModel(tc.src, "sonnet")
			if out != tc.want || !slices.Equal(amb, tc.ambiguous) {
				t.Errorf("FillModel(%q) = %q, %v; want %q, %v", tc.src, out, amb, tc.want, tc.ambiguous)
			}
		})
	}
}

// useNode makes lookNode find a real node (hosttest strips PATH; PLANLINT_NODE names the binary) or skips.
func useNode(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("node"); err == nil {
		return
	}
	node := os.Getenv("PLANLINT_NODE")
	if node == "" {
		t.Skip("node not on PATH and PLANLINT_NODE unset")
	}
	orig := lookNode
	lookNode = func() (string, error) { return node, nil }
	t.Cleanup(func() { lookNode = orig })
}

// noNode hides node so a test of the pin and registry checks never depends on the host's node.
func noNode(t *testing.T) {
	t.Helper()
	orig := lookNode
	lookNode = func() (string, error) { return "", exec.ErrNotFound }
	t.Cleanup(func() { lookNode = orig })
}

// The effort pin allow-list keeps its pinned contents; an agentid rename fails here.
func TestPinAllowListTiesToAgentid(t *testing.T) {
	wantEffort := map[string]bool{"low": true, "medium": true, "high": true, "xhigh": true, "max": true}
	if !maps.Equal(pinAllowed["effort"], wantEffort) {
		t.Errorf("pinAllowed[effort] = %v, want %v", pinAllowed["effort"], wantEffort)
	}
}
