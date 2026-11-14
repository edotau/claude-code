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

type wfM = map[string]any

// wfLintPayload builds the PreToolUse Workflow stdin JSON for an inline, saved or by-name script.
func wfLintPayload(t *testing.T, extra wfM) string {
	t.Helper()
	b, err := json.Marshal(wfM{"tool_name": "Workflow", "tool_input": maps.Clone(extra)})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// runWorkflowLint renders WorkflowResult like the emitter: block reason on stderr, else updatedInput on stdout.
func runWorkflowLint(stdin string) (code int, stdout, stderr string) {
	res := WorkflowResult(core.ParseInput(strings.NewReader(stdin)))
	if res.Decision == core.Block {
		return core.ExitBlock, "", res.Reason + "\n"
	}
	if res.UpdatedInput == nil {
		return core.ExitProceed, "", ""
	}
	inner := wfM{"hookEventName": "PreToolUse", "updatedInput": res.UpdatedInput}
	if res.Context != "" {
		inner["additionalContext"] = res.Context
	}
	out, _ := json.Marshal(wfM{"hookSpecificOutput": inner})
	return core.ExitProceed, string(out) + "\n", ""
}

type wfLintInlineDoc struct {
	HSO struct {
		AdditionalContext string `json:"additionalContext"`
		UpdatedInput      wfM    `json:"updatedInput"`
	} `json:"hookSpecificOutput"`
}

// decodeWfLintInline parses the single JSON line a rewrite writes to stdout.
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

func wfLintArgsK1(t *testing.T, doc wfLintInlineDoc) {
	t.Helper()
	if args, ok := doc.HSO.UpdatedInput["args"].(wfM); !ok || args["k"] != float64(1) {
		t.Errorf("updatedInput.args = %v, want {k:1} echoed back", doc.HSO.UpdatedInput["args"])
	}
}

// wfLintConfig points CLAUDE_CONFIG_DIR at a fresh dir; a non-nil body installs workflows/gate-loop.js there.
func wfLintConfig(t *testing.T, body *string) (dir, path string) {
	t.Helper()
	dir = t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	if body == nil {
		return dir, ""
	}
	path = writeScript(t, mkdirAll(t, filepath.Join(dir, "workflows")), "gate-loop.js", *body)
	return dir, path
}

func mkdirAll(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func readScript(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// wfLintSaved writes body at file under a fresh root and returns the path; file "-" writes nothing.
func wfLintSaved(t *testing.T, file, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), file)
	if file != "-" {
		writeScript(t, mkdirAll(t, filepath.Dir(path)), filepath.Base(path), body)
	}
	return path
}

// Inline and saved scripts: block names the file or the defect, proceed is silent, a saved script is never rewritten.
func TestWorkflowLintInlineAndSaved(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	cases := []struct {
		name    string
		script  string // inline script; ignored when file is set
		file    string // saved script's path under the temp root; "-" = absent on disk
		body    string
		block   bool
		wantErr []string // "{path}" expands to the saved script path
	}{
		{name: "inline fully pinned", script: "agent('x', { model: 'sonnet' });\n"},
		{name: "inline string mention not miscounted", script: "export const meta = { description: 'calls agent() with pins' };\nagent('x', { model: 'sonnet' });\n"},
		{name: "inline ambiguous options", script: "const opts = { model: 'sonnet' };\nagent('x', opts);\n", block: true, wantErr: []string{"ambiguous", "line"}},
		{name: "inline bad model value", script: "agent('x', { model: 'gpt-5' });\n", block: true, wantErr: []string{"gpt-5"}},
		{name: "saved unpinned", file: "saved.js", body: "agent('x');\n", block: true, wantErr: []string{"{path}"}},
		{name: "saved under skills/scripts", file: "skills/problem-solving/scripts/code-evaluator.js", block: true, wantErr: []string{"{path}"},
			body: "export const meta = { name: 'code-evaluator' }\nagent('x');\n"},
		{name: "saved missing effort only", file: "saved.js", body: "agent('x', { model: 'sonnet' });\n"},
		{name: "saved missing on disk fails open", file: "-"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in, path := wfM{"script": c.script}, ""
			if c.file != "" {
				path = wfLintSaved(t, c.file, c.body)
				in = wfM{"scriptPath": path}
			}
			code, out, errb := runWorkflowLint(wfLintPayload(t, in))
			want := core.ExitProceed
			if c.block {
				want = core.ExitBlock
			}
			if code != want || out != "" {
				t.Fatalf("exit = %d (want %d) stdout=%q stderr=%q, want empty stdout", code, want, out, errb)
			}
			if !c.block && errb != "" {
				t.Errorf("stderr = %q, want empty", errb)
			}
			for _, w := range c.wantErr {
				if w = strings.ReplaceAll(w, "{path}", path); !strings.Contains(errb, w) {
					t.Errorf("stderr = %q, want it to contain %q", errb, w)
				}
			}
		})
	}
}

func TestWorkflowLintNonWorkflowToolProceeds(t *testing.T) {
	code, out, errb := runWorkflowLint(`{"tool_name":"Agent","tool_input":{}}`)
	if code != core.ExitProceed || out != "" || errb != "" {
		t.Fatalf("exit=%d stdout=%q stderr=%q, want ExitProceed and both empty", code, out, errb)
	}
}

func TestWorkflowLintInlineUnpinnedFillsModel(t *testing.T) {
	script := "// comment\n// comment2\nagent('x', { label: 'l' });\n"
	code, out, errb := runWorkflowLint(wfLintPayload(t, wfM{"script": script, "args": wfM{"k": 1}}))
	if code != core.ExitProceed {
		t.Fatalf("exit = %d, want ExitProceed (stderr=%q)", code, errb)
	}
	doc := decodeWfLintInline(t, out)
	got, _ := doc.HSO.UpdatedInput["script"].(string)
	for _, w := range []string{"model: 'sonnet'", "label: 'l'"} {
		if !strings.Contains(got, w) {
			t.Errorf("updatedInput.script = %q, want it to contain %s", got, w)
		}
	}
	if !strings.Contains(doc.HSO.AdditionalContext, "line(s) 3") {
		t.Errorf("additionalContext = %q, want it to mention line(s) 3", doc.HSO.AdditionalContext)
	}
	wfLintArgsK1(t, doc)
}

// By name: an unknown name proceeds silently, a resolved unpinned file blocks naming it.
func TestWorkflowLintByNameNotInlined(t *testing.T) {
	unpinned := "agent('x');\n"
	cases := []struct {
		name  string
		body  *string
		block bool
	}{{"unknown proceeds", nil, false}, {"resolved file unpinned blocks", &unpinned, true}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, path := wfLintConfig(t, c.body)
			code, out, errb := runWorkflowLint(wfLintPayload(t, wfM{"name": "gate-loop", "args": wfM{}}))
			if out != "" || !c.block && (code != core.ExitProceed || errb != "") || c.block && (code != core.ExitBlock || !strings.Contains(errb, path)) {
				t.Fatalf("exit=%d stdout=%q stderr=%q, want block=%v naming %q", code, out, errb, c.block, path)
			}
		})
	}
}

func TestWorkflowLintByNameInlinesScript(t *testing.T) {
	body := "agent('x', { model: 'sonnet' });\n"
	_, path := wfLintConfig(t, &body)
	code, out, errb := runWorkflowLint(wfLintPayload(t, wfM{"name": "gate-loop", "args": wfM{"k": 1}}))
	if code != core.ExitProceed {
		t.Fatalf("exit = %d, want ExitProceed (stderr=%q)", code, errb)
	}
	doc := decodeWfLintInline(t, out)
	if got, _ := doc.HSO.UpdatedInput["script"].(string); got != body {
		t.Errorf("updatedInput.script = %q, want the file's text inlined", got)
	}
	for _, k := range []string{"scriptPath", "name"} {
		if _, ok := doc.HSO.UpdatedInput[k]; ok {
			t.Errorf("updatedInput still carries %q, want the inline script only (the tool refuses out-of-cwd paths)", k)
		}
	}
	wfLintArgsK1(t, doc)
	if ctx := doc.HSO.AdditionalContext; !strings.Contains(ctx, "gate-loop") || !strings.Contains(ctx, path) {
		t.Errorf("additionalContext = %q, want it to mention the name and resolved path", ctx)
	}
}

// Auto mode can't review a rewritten call: a by-name call under the cwd blocks with the scriptPath re-issue.
func TestWorkflowLintAutoModeByNameUnderCwdBlocksWithScriptPath(t *testing.T) {
	body := "agent('x', { model: 'sonnet' });\n"
	dir, path := wfLintConfig(t, &body)
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

func TestCheckWorkflowSteps(t *testing.T) {
	orig := stepRoute
	stepRoute = func(who, _, profile, _ string) error {
		if profile == "nope" {
			return errors.New("profile nope is not in the registry")
		}
		return nil
	}
	t.Cleanup(func() { stepRoute = orig })
	steps := func(stage string, opts wfM) wfM { return wfM{"steps": wfM{stage: opts}} }
	cases := []struct {
		script string
		args   any
		want   []string // substrings, one per expected problem; empty = the call may run
	}{
		{"code-workers.js", steps("fix", wfM{"effort": "huge"}), []string{"steps.fix.effort must be"}},
		{"code-workers.js", steps("nope", wfM{}), []string{"steps.nope: unknown stage (allowed: dedupe|find|fix|gate|rederive|review|simplify|test)"}},
		{"code-workers.js", steps("find", wfM{"model": "gpt-5", "agent": "bard"}), []string{"steps.find.agent must be"}},
		{"code-workers.js", steps("find", wfM{"model": "gpt-5"}), []string{"steps.find.model must be haiku|sonnet|opus|fable"}},
		{"code-workers.js", steps("find", wfM{"profile": "openai:x", "agent": "codex"}), []string{"must be a plain name"}},
		{"code-workers.js", `{"steps":{"review":{"agent":"codex","profile":"nope"}}}`, []string{"steps.review: codex has no route: profile nope"}},
		{"code-workers.js", `{"steps":{"bogus":{}}`, []string{"args must be a JSON object"}},
		{"code-workers.js", wfM{"profile": "nope", "steps": wfM{"review": wfM{"agent": "codex"}}}, []string{"codex has no route"}},
		{"code-engineer.js", wfM{"steps": wfM{"child": wfM{"bogus": wfM{}}, "size": wfM{"profile": "qa"}}},
			[]string{"steps.child.bogus: unknown stage"}},
		{"code-workers.js", wfM{"steps": wfM{"fix": wfM{"model": "opus", "effort": "high"}, "review": wfM{"agent": "codex"}}}, nil},
		{"code-engineer.js", wfM{"steps": wfM{"impl": wfM{"model": "opus"}, "child": wfM{"fix": wfM{"effort": "high"}}}}, nil},
		{"code-workers.js", steps("fix", wfM{"agent": "claude", "profile": "nope"}), []string{"claude has no route"}},
		{"code-workers.js", wfM{"profile": "nope", "steps": wfM{"fix": wfM{}}}, []string{"claude has no route"}},
		{"code-workers.js", steps("fix", wfM{"profile": "qa", "provider": "databricks"}), nil},
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
	t.Parallel()
	if got, want := fieldProblem("agent", "x"), " must be claude|codex|copilot|gemini|opencode"; got != want {
		t.Errorf("agent problem %q, want %q", got, want)
	}
	if got, want := fieldProblem("effort", "x"), " must be low|medium|high|xhigh|max"; got != want {
		t.Errorf("effort problem %q, want %q", got, want)
	}
}

// Hits come back as path:line; a model pin does not satisfy effort, and opus-ok and word-prefixed calls are skipped.
func TestEffortlessAgentCalls(t *testing.T) {
	t.Parallel()
	p := writeScript(t, t.TempDir(), "wf.js", `const a = await agent('task one', { label: 'x', effort: 'low' })
const b = await agent('task two', { label: 'y', model: 'sonnet' })
const c = await agent('judge', { label: 'j' }) // lint:opus-ok
const d = await subagent('not the primitive')
`)
	if hits := effortlessAgentCalls(p, readScript(t, p)); !slices.Equal(hits, []string{p + ":2"}) {
		t.Fatalf("hits = %v, want exactly the line-2 call (a model pin does not imply effort)", hits)
	}
}

// Only a model: pin satisfies it: agentType alone does not (a built-in Explore leaf ran opus under one).
func TestUnpinnedAgentCalls(t *testing.T) {
	t.Parallel()
	p := writeScript(t, t.TempDir(), "wf.js", `const a = await agent('task one', { label: 'x', schema: S })
const b = await agent('task two', { label: 'y', model: 'sonnet' })
const c = await agent('task three', { agentType: 'claude' })
const d = await agent('judge', { label: 'j' }) // lint:opus-ok
const e = await subagent('not the primitive')
`)
	if hits := unpinnedAgentCalls(p, readScript(t, p)); !slices.Equal(hits, []string{p + ":1", p + ":3", p + ":4"}) {
		t.Fatalf("hits = %v, want lines 1, 3 and 4 (agentType alone and opus-ok do not pin a model)", hits)
	}
}

// Prompt text containing "effort:"/"model:" must not satisfy the pin check, nor a paren end the call window.
func TestStringLiteralDoesNotMaskPin(t *testing.T) {
	t.Parallel()
	p := writeScript(t, t.TempDir(), "wf.js", "const a = await agent('task', { prompt: \"remember effort: low later, model: sonnet too\" })\n"+
		"const b = await agent(`run f(x) then ) report`, { model: 'sonnet', effort: 'low' })\n")
	src := readScript(t, p)
	if hits := effortlessAgentCalls(p, src); !slices.Equal(hits, []string{p + ":1"}) {
		t.Fatalf("effortlessAgentCalls = %v, want only line 1", hits)
	}
	if hits := unpinnedAgentCalls(p, src); !slices.Equal(hits, []string{p + ":1"}) {
		t.Fatalf("unpinnedAgentCalls = %v, want only line 1", hits)
	}
}

// A read error must surface as a hit, not silently report the file clean.
func TestUnreadableFileIsAHit(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "does-not-exist.js")
	if hits := registryHidingDefects(missing); len(hits) != 1 || !strings.Contains(hits[0], "cannot read file") {
		t.Errorf("registryHidingDefects(missing) = %v, want one 'cannot read file' hit", hits)
	}
}

// A non-literal meta and a top-level throw both hide a script from the registry; indented throws and clean metas pass.
func TestRegistryHidingDefects(t *testing.T) {
	t.Parallel()
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
	if got := registryHidingDefects(thrown); len(got) != 2 || !strings.Contains(got[0], "throw.js:5") || !strings.Contains(got[1], "throw.js:6") {
		t.Errorf("top-level throws: got %v, want hits on lines 5 and 6", got)
	}
}

// An apostrophe in a comment must not open a string and blank the pin after it.
func TestStripStringsSkipsComments(t *testing.T) {
	t.Parallel()
	got := stripStrings("agent('x', {\n  // don't inherit session effort\n  effort: 'high',\n  /* it's pinned */ model: 'sonnet' })")
	if !strings.Contains(got, "effort:") || !strings.Contains(got, "model:") {
		t.Fatalf("stripStrings blanked a real pin: %q", got)
	}
}

// An apostrophe in a comment inside agent({...}) must not open a string and swallow the close paren.
func TestCallWindowSkipsCommentApostrophe(t *testing.T) {
	t.Parallel()
	src := "agent('x', { // don't wait\n model: 'sonnet' })\nagent('y', { model: 'opus' })"
	if got := delimitedWindow(src, stripStrings(src), 0, '(', ')'); strings.Contains(got, "opus") {
		t.Fatalf("window %q spilled into the next call", got)
	}
}

func lintVerb(root string) (rc int, out, errs string) {
	var o, e bytes.Buffer
	rc = RunWorkflowLint([]string{root}, &o, &e)
	return rc, o.String(), e.String()
}

// The verb, for workflows/*.js and skills/*/scripts/*.js alike: clean exits 0 with the count, a flagged one exits 1.
func TestRunWorkflowLint(t *testing.T) {
	noNode(t)
	for _, rel := range []string{"workflows", filepath.Join("skills", "s", "scripts")} {
		t.Run(rel, func(t *testing.T) {
			root := t.TempDir()
			dir := mkdirAll(t, filepath.Join(root, rel))
			writeScript(t, dir, "ok.js", "export const meta = { name: 'ok' }\nawait agent('x', { model: 'sonnet', effort: 'low' })\n")
			if rc, out, errs := lintVerb(root); rc != 0 || out != "workflow lint: 1 script(s) clean\n" {
				t.Fatalf("clean: rc=%d out=%q err=%q", rc, out, errs)
			}
			writeScript(t, dir, "bad.js", "export const meta = { name: 'bad' }\nawait agent('x', { label: 'l' })\n")
			rc, out, errs := lintVerb(root)
			if rc != 1 || strings.Count(errs, "bad.js:2") != 2 || out != "" ||
				!strings.Contains(errs, "agent() without model:") || !strings.Contains(errs, "agent() without effort:") {
				t.Fatalf("flagged: rc=%d out=%q err=%q", rc, out, errs)
			}
		})
	}
}

// A top-level throw hides the script from the registry: the verb reports it at its path and line and exits 1.
func TestRunWorkflowLintReportsTopLevelThrow(t *testing.T) {
	noNode(t)
	dir := mkdirAll(t, filepath.Join(t.TempDir(), "workflows"))
	p := writeScript(t, dir, "throws.js", "export const meta = { name: 'throws' }\nthrow new Error('boom')\n")
	rc, out, errs := lintVerb(filepath.Dir(dir))
	if rc == 0 || !strings.Contains(errs, p+":2: top-level throw") || out != "" {
		t.Fatalf("rc=%d out=%q err=%q; want non-zero and %s:2: top-level throw", rc, out, errs, p)
	}
}

// A root with no workflows/*.js is an error naming the searched path, not "0 script(s) clean".
func TestRunWorkflowLintNoScripts(t *testing.T) {
	root := t.TempDir()
	want := filepath.Join(root, "workflows")
	if rc, out, errs := lintVerb(root); rc != 1 || !strings.Contains(errs, want) || out != "" {
		t.Fatalf("empty root: rc=%d out=%q err=%q; want 1, stderr naming %s", rc, out, errs, want)
	}
}

// isWorkflowScript excludes a plain helper (even with an unpinned agent()) and includes one with a comment before meta.
func TestIsWorkflowScript(t *testing.T) {
	t.Parallel()
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

// A literal pin outside the allow-list is flagged; ternary/identifier values and string text pass; opus-ok exempts all.
func TestBadPinValues(t *testing.T) {
	t.Parallel()
	p := writeScript(t, t.TempDir(), "wf.js", "agent('a', { model: 'gpt-5', effort: 'low' })\n"+
		"agent('b', { model: 'sonnet', effort: 'med' })\n"+
		"agent('c', { model: hot ? 'opus' : 'sonnet', effort: x.effort || 'low' })\n"+
		"agent('d', { model: s.model, effort: s.effort })\n"+
		"agent('e', { label: \"model: gpt-5\", model: 'haiku', effort: 'high' })\n"+
		"agent('f', { model: 'fable', effort: 'high' }) // lint:opus-ok\n")
	hits := badPinValues(p, readScript(t, p))
	if len(hits) != 2 {
		t.Fatalf("hits = %v, want exactly 2 (line 1 gpt-5, line 2 med)", hits)
	}
	for i, w := range [][]string{{p + ":1:", "model", `"gpt-5"`}, {p + ":2:", "effort", `"med"`}} {
		for _, s := range w {
			if !strings.Contains(hits[i], s) {
				t.Errorf("hits[%d] = %q, want it to contain %q", i, hits[i], s)
			}
		}
	}
}

// routeTier('<tier>') still counts as pinned and its literal is validated; routeTier(cfg.x) has no literal, so it
// passes (current lenient behavior).
func TestRouteTierWrappedModelPin(t *testing.T) {
	t.Parallel()
	if hits := LintPins("inline", []byte("agent('a', { model: routeTier('sonnet'), effort: 'low' })\n")); len(hits) != 0 {
		t.Fatalf("LintPins(routeTier('sonnet')) = %v, want none — the wrapped literal is a valid pin", hits)
	}
	if hits := LintPins("inline", []byte("agent('a', { model: routeTier('gpt-5'), effort: 'low' })\n")); len(hits) != 1 || !strings.Contains(hits[0], `"gpt-5"`) {
		t.Fatalf("LintPins(routeTier('gpt-5')) = %v, want one hit naming value gpt-5", hits)
	}
	if hits := LintPins("inline", []byte("agent('a', { model: routeTier(cfg.x), effort: 'low' })\n")); len(hits) != 0 {
		t.Fatalf("LintPins(routeTier(cfg.x)) = %v, want none — no literal to validate, and model: text alone pins it", hits)
	}
}

// lint:opus-ok exempts only the effort axis — a marked call missing model: is still a model hit.
func TestOpusOKExemptsEffortOnly(t *testing.T) {
	t.Parallel()
	hits := LintPins("inline", []byte("await agent('x', { label: 'l' }) // lint:opus-ok\n"))
	if len(hits) != 1 {
		t.Fatalf("hits = %v, want exactly 1 (model hit, no effort hit)", hits)
	}
	if !strings.Contains(hits[0], "without model:") || strings.Contains(hits[0], "effort:") {
		t.Errorf("hits[0] = %q, want the model-missing message only", hits[0])
	}
}

// Anchored on this file's path so a cwd change can never make the guard skip instead of gate.
func TestShippedWorkflowsAreClean(t *testing.T) {
	useNode(t)
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed — cannot locate the repo root")
	}
	repoRoot := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(thisFile))))
	if rc, out, errs := lintVerb(repoRoot); rc != 0 || !strings.Contains(out, "script(s) clean") || strings.HasPrefix(out, "workflow lint: 0 ") {
		t.Fatalf("shipped workflows under %s: rc=%d out=%q\n%s", repoRoot, rc, out, errs)
	}
}

func TestAgentCallWhitespaceAndIdentifierBoundary(t *testing.T) {
	t.Parallel()
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
			t.Parallel()
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

// fakeNode swaps lookNode for a sh stand-in running body, or skips where none can run.
func fakeNode(t *testing.T, dir, body string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs a sh stand-in for node")
	}
	fake := writeScript(t, dir, "fakenode", body)
	if err := os.Chmod(fake, 0o755); err != nil {
		t.Fatal(err)
	}
	orig := lookNode
	lookNode = func() (string, error) { return fake, nil }
	t.Cleanup(func() { lookNode = orig })
}

// One unreadable script is one defect: lintScript reports it, node is never handed it to re-report as a parse error.
func TestUnreadableScriptReportedOnce(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a sh stand-in for node")
	}
	root := t.TempDir()
	wf := mkdirAll(t, filepath.Join(root, "workflows"))
	if err := os.Symlink(filepath.Join(root, "gone.js"), filepath.Join(wf, "dangling.js")); err != nil {
		t.Skip("symlinks unavailable")
	}
	fakeNode(t, root, "#!/bin/sh\nshift 2\nfor f; do echo \"$f: ENOENT\"; done\nexit 1\n")
	rc, _, got := lintVerb(root)
	if rc != 1 || strings.Count(got, "cannot read file") != 1 || strings.Contains(got, "does not parse") {
		t.Fatalf("rc = %d stderr = %q, want rc 1, one 'cannot read file' and no 'does not parse'", rc, got)
	}
}

// Only parseCheckJS's own "<file>: <message>" lines are parse failures; node warnings beside them are not relabelled.
func TestParseDefectsIgnoreNodeNoise(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, "a.js", "x\n")
	fakeNode(t, dir, "#!/bin/sh\necho '(node:1) Warning: noisy'\necho '"+script+": boom'\nexit 1\n")
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
	t.Parallel()
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
			t.Parallel()
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
	t.Parallel()
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
			t.Parallel()
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
	t.Parallel()
	wantEffort := map[string]bool{"low": true, "medium": true, "high": true, "xhigh": true, "max": true}
	if !maps.Equal(pinAllowed["effort"], wantEffort) {
		t.Errorf("pinAllowed[effort] = %v, want %v", pinAllowed["effort"], wantEffort)
	}
}

// Execute each copied selector: native stages stay native; routed stages preserve tool tiers and receipts.
func TestWorkflowClaudeProfileLaunch(t *testing.T) {
	useNode(t)
	node, err := lookNode()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"code-analyzer", "code-engineer", "code-evaluator", "code-workers", "green-keeper-loop"} {
		t.Run(name, func(t *testing.T) {
			src, _ := realScript(t, name+".js")
			start := strings.Index(string(src), helperStart)
			end := strings.Index(string(src), helperEnd)
			if start < 0 || end < start {
				t.Fatal("missing stepOf block")
			}
			cmd := exec.Command(node, "--input-type=module", "-e", string(src[start:end])+workflowLaunchProof)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("selector: %v\n%s", err, out)
			}
		})
	}
}

const workflowLaunchProof = `
const log = () => {}
const assert = (ok, why) => { if (!ok) throw new Error(why) }
const opt = {allowed:['fix'], external:true}
const native = stepOf({}, 'fix', {tools:'edit'}, opt)
assert(!native.ext && native.task('work') === 'work', 'native stage changed')
for (const tools of ['edit', 'run', undefined]) {
  for (const inherited of [false, true]) {
    const route = {profile:'qa', provider:'databricks'}
    const stage = stepOf({fix: inherited ? {} : {agent:'claude', ...route}}, 'fix', {tools},
      inherited ? {...opt, ...route} : opt)
    const task = stage.task('work')
    assert(stage.ext && task.includes('claude-code agents ask --agent claude'), 'profile ignored')
    assert(task.includes('--profile qa --provider databricks') && task.includes('--tools'), 'route/tools lost')
    assert(task.includes('task is read-only') === (tools !== 'edit'), 'edit tier changed')
    assert(stage.done('fix', {coworker:'ok', ran_on:'claude qa:model'}) !== null, 'receipt lost')
  }
}
let refused = false
try { stepOf({fix:{profile:'qa'}}, 'fix', {}, {...opt, external:false}) }
catch { refused = true }
assert(refused, 'unwired route allowed')
`
