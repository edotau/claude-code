package planlint

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

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
	if os.Geteuid() == 0 || runtime.GOOS == "windows" {
		t.Skip("root and windows read mode-000 files")
	}
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := writeScript(t, filepath.Join(root, "workflows"), "locked.js", "await agent('x', { model: 'sonnet', effort: 'low' })\n")
	if err := os.Chmod(p, 0); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := RunWorkflowLint([]string{root}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "cannot read file") {
		t.Fatalf("RunWorkflowLint(unreadable) = %d, stderr %q; want 1 and a 'cannot read file' line", code, errb.String())
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

// The verb: a clean tree reports the count on stdout and exits 0; a flagged one lists hits on stderr and exits 1.
func TestRunWorkflowLint(t *testing.T) {
	useNode(t)
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeScript(t, filepath.Join(root, "workflows"), "ok.js", "export const meta = { name: 'ok' }\nawait agent('x', { model: 'sonnet', effort: 'low' })\n")
	var out, errb bytes.Buffer
	if rc := RunWorkflowLint([]string{root}, &out, &errb); rc != 0 || !strings.Contains(out.String(), "1 script(s) clean") {
		t.Fatalf("clean: rc=%d out=%q err=%q", rc, out.String(), errb.String())
	}
	writeScript(t, filepath.Join(root, "workflows"), "bad.js", "export const meta = { name: 'bad' }\nawait agent('x', { label: 'l' })\n")
	out.Reset()
	errb.Reset()
	if rc := RunWorkflowLint([]string{root}, &out, &errb); rc != 1 || strings.Count(errb.String(), "bad.js:2") != 2 {
		t.Fatalf("flagged: rc=%d err=%q", rc, errb.String())
	}
}

// RunWorkflowLint treats a skills/<x>/scripts/<y>.js that opens with `export const meta` the same as
// a workflows/*.js: clean passes, an unpinned agent() call is flagged on stderr and fails the run.
func TestRunWorkflowLintSkillsScripts(t *testing.T) {
	useNode(t)
	root := t.TempDir()
	cleanDir := filepath.Join(root, "skills", "clean-skill", "scripts")
	if err := os.MkdirAll(cleanDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeScript(t, cleanDir, "run.js", "export const meta = { name: 'run' }\nawait agent('x', { model: 'sonnet', effort: 'low' })\n")
	var out, errb bytes.Buffer
	if rc := RunWorkflowLint([]string{root}, &out, &errb); rc != 0 || !strings.Contains(out.String(), "1 script(s) clean") {
		t.Fatalf("clean: rc=%d out=%q err=%q", rc, out.String(), errb.String())
	}

	badDir := filepath.Join(root, "skills", "bad-skill", "scripts")
	if err := os.MkdirAll(badDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeScript(t, badDir, "run.js", "export const meta = { name: 'run' }\nawait agent('x', { label: 'l' })\n")
	out.Reset()
	errb.Reset()
	if rc := RunWorkflowLint([]string{root}, &out, &errb); rc != 1 || strings.Count(errb.String(), "run.js:2") != 2 {
		t.Fatalf("flagged: rc=%d err=%q", rc, errb.String())
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

// LintPins on raw bytes: an unpinned call yields model-missing then effort-missing, both on line 1;
// a fully pinned call yields nothing.
func TestLintPinsBytes(t *testing.T) {
	hits := LintPins("inline", []byte("await agent('x', { label: 'l' })\n"))
	if len(hits) != 2 {
		t.Fatalf("hits = %v, want exactly 2 messages", hits)
	}
	if !strings.HasPrefix(hits[0], "inline:1") || !strings.Contains(hits[0], "without model:") {
		t.Errorf("hits[0] = %q, want the model-missing message first", hits[0])
	}
	if !strings.HasPrefix(hits[1], "inline:1") || !strings.Contains(hits[1], "effort:") {
		t.Errorf("hits[1] = %q, want the effort-missing message second", hits[1])
	}
	pinned := LintPins("inline", []byte("await agent('x', { model: 'sonnet', effort: 'low' })\n"))
	if len(pinned) != 0 {
		t.Fatalf("fully pinned call: hits = %v, want none", pinned)
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

// A live bug: an unpinned-looking "agent()" mentioned inside a string (e.g. a meta description) must
// not be read as a call by either LintPins or FillModel — only the real, pinned call below it counts.
func TestAgentInStringIsNotACall(t *testing.T) {
	src := "export const meta = { description: 'an unpinned agent() probe' }\n" +
		"await agent('x', { model: 'sonnet', effort: 'low' })\n"
	if hits := LintPins("inline", []byte(src)); len(hits) != 0 {
		t.Fatalf("LintPins = %v, want none (the agent() text is inside a string)", hits)
	}
	out, ambiguous := FillModel(src, "sonnet")
	if out != src {
		t.Errorf("FillModel rewrote the source; out = %q, want unchanged", out)
	}
	if len(ambiguous) != 0 {
		t.Errorf("ambiguous = %v, want none", ambiguous)
	}
}

// FillModel: each test covers one shape the rewrite must handle (insert, empty object, one-arg
// append, already pinned, non-object options) or correctly refuse (identifier/spread/ternary options).

// Label-only object gets model inserted, existing key survives
func TestFillModelLabelOnlyObject(t *testing.T) {
	out, ambiguous := FillModel("agent('p', { label: 'l' })\n", "sonnet")
	if !strings.Contains(out, "model: 'sonnet',") {
		t.Errorf("out = %q, want an inserted model: 'sonnet',", out)
	}
	if !strings.Contains(out, "label: 'l'") {
		t.Errorf("out = %q, want the original label: 'l' to survive", out)
	}
	if len(ambiguous) != 0 {
		t.Errorf("ambiguous = %v, want none", ambiguous)
	}
}

// Empty object gets model with no stray comma
func TestFillModelEmptyObject(t *testing.T) {
	out, ambiguous := FillModel("agent('p', {})\n", "sonnet")
	if stripped := strings.ReplaceAll(out, " ", ""); !strings.Contains(stripped, "{model:'sonnet'}") {
		t.Errorf("out = %q, want {model: 'sonnet'} (any spacing)", out)
	}
	if len(ambiguous) != 0 {
		t.Errorf("ambiguous = %v, want none", ambiguous)
	}
}

// One-arg call gets an options object appended
func TestFillModelOneArgCall(t *testing.T) {
	out, ambiguous := FillModel("agent('p')\n", "sonnet")
	if !strings.Contains(out, ", { model: 'sonnet' }") {
		t.Errorf("out = %q, want an appended , { model: 'sonnet' }", out)
	}
	if len(ambiguous) != 0 {
		t.Errorf("ambiguous = %v, want none", ambiguous)
	}
}

// Already pinned calls are left unchanged
func TestFillModelAlreadyPinned(t *testing.T) {
	for _, src := range []string{
		"agent('p', { model: 'opus' })\n",
		"agent('p', { agentType: 'claude', model: 'sonnet' })\n",
	} {
		out, ambiguous := FillModel(src, "sonnet")
		if out != src {
			t.Errorf("src = %q: out = %q, want unchanged", src, out)
		}
		if len(ambiguous) != 0 {
			t.Errorf("src = %q: ambiguous = %v, want none", src, ambiguous)
		}
	}
}

// Options as a bare identifier is ambiguous, unchanged
func TestFillModelBareIdentifierAmbiguous(t *testing.T) {
	src := "agent('p', opts)\n"
	out, ambiguous := FillModel(src, "sonnet")
	if out != src {
		t.Errorf("out = %q, want unchanged", out)
	}
	if len(ambiguous) != 1 || ambiguous[0] != "line 1" {
		t.Errorf("ambiguous = %v, want exactly [\"line 1\"]", ambiguous)
	}
}

// Spread or ternary options are ambiguous, unchanged
func TestFillModelSpreadTernaryAmbiguous(t *testing.T) {
	for _, src := range []string{
		"agent('p', ...opts)\n",
		"agent('p', cond ? a : b)\n",
	} {
		out, ambiguous := FillModel(src, "sonnet")
		if out != src {
			t.Errorf("src = %q: out = %q, want unchanged", src, out)
		}
		if len(ambiguous) != 1 || ambiguous[0] != "line 1" {
			t.Errorf("src = %q: ambiguous = %v, want exactly [\"line 1\"]", src, ambiguous)
		}
	}
}

// Two calls on different lines both filled with correct back-to-front offsets
func TestFillModelTwoCallsBackToFront(t *testing.T) {
	src := "agent('a', { label: 'x' })\nagent('b', { label: 'y' })\n"
	out, ambiguous := FillModel(src, "sonnet")
	if len(ambiguous) != 0 {
		t.Fatalf("ambiguous = %v, want none", ambiguous)
	}
	if n := strings.Count(out, "model: 'sonnet',"); n != 2 {
		t.Errorf("out = %q, want 2 insertions, got %d", out, n)
	}
	if strings.Count(out, "\n") != strings.Count(src, "\n") {
		t.Errorf("out = %q changed the line count", out)
	}
}

// A template literal mentioning agent( and model: is neither a call nor a pin
func TestFillModelIgnoresTemplateLiteral(t *testing.T) {
	src := "const prompt = `call agent(x) with model: sonnet`\nagent('p', { label: 'l' })\n"
	out, ambiguous := FillModel(src, "sonnet")
	if len(ambiguous) != 0 {
		t.Fatalf("ambiguous = %v, want none (the template text is not a call)", ambiguous)
	}
	if n := strings.Count(out, "model: 'sonnet',"); n != 1 {
		t.Errorf("out = %q, want exactly 1 real insertion, got %d", out, n)
	}
	if !strings.Contains(out, "call agent(x) with model: sonnet") {
		t.Errorf("out = %q, template literal text must survive untouched", out)
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
	repoRoot := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))) // internal/review/planlint/x_test.go -> repo
	var out, errb bytes.Buffer
	if rc := RunWorkflowLint([]string{repoRoot}, &out, &errb); rc != 0 || !strings.Contains(out.String(), "script(s) clean") || strings.HasPrefix(out.String(), "workflow lint: 0 ") {
		t.Fatalf("shipped workflows under %s: rc=%d out=%q\n%s", repoRoot, rc, out.String(), errb.String())
	}
}

func TestAgentPinsIgnoreNonOptionsObjects(t *testing.T) {
	for _, src := range []string{
		"agent('p', [{ model: 'gpt-5', effort: 'med' }])",
		"agent('p', makeOptions({ model: 'gpt-5', effort: 'med' }))",
	} {
		t.Run(src, func(t *testing.T) {
			hits := LintPins("inline", []byte(src))
			if len(hits) != 2 {
				t.Fatalf("LintPins = %v, want only missing model and effort (not nested value errors)", hits)
			}
			if !strings.Contains(hits[0], "without model:") || !strings.Contains(hits[1], "without effort:") {
				t.Errorf("LintPins = %v, want missing model then effort", hits)
			}
		})
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

func TestRegistryHidingSkipsMethodBodies(t *testing.T) {
	for _, method := range []string{
		"const obj = { run(x) { if (x) { throw new Error('deferred') } } }",
		"class Worker { run(x) { if (x) { throw new Error('deferred') } } }",
	} {
		t.Run(method, func(t *testing.T) {
			src := "export const meta = { name: 'x' }\n" + method + "\nif (ready()) { throw new Error('import') }\n"
			p := writeScript(t, t.TempDir(), "methods.js", src)
			hits := registryHidingDefects(p)
			if len(hits) != 1 || !strings.HasPrefix(hits[0], p+":3:") || !strings.Contains(hits[0], "top-level throw") {
				t.Errorf("registryHidingDefects = %v, want only the line-3 module throw", hits)
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
	if got := callWindow(src, 0); strings.Contains(got, "opus") {
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
	orig := lookNode
	lookNode = func() (string, error) { return "", exec.ErrNotFound }
	t.Cleanup(func() { lookNode = orig })
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
	node, err := exec.LookPath("node")
	if err != nil {
		node = os.Getenv("PLANLINT_NODE") // hosttest strips PATH
	}
	if node == "" {
		t.Skip("node not on PATH and PLANLINT_NODE unset")
	}
	orig := lookNode
	lookNode = func() (string, error) { return node, nil }
	t.Cleanup(func() { lookNode = orig })
	p := writeScript(t, t.TempDir(), "two.js", "export const meta = {}\nexport const x = 1\n")
	if hits, _ := workflowParseDefects([]string{p}); len(hits) != 0 {
		t.Fatalf("two exports reported as parse defect: %v", hits)
	}
}

// With no node the parse check cannot run, so the verb must exit 1 rather than print clean.
// A throw inside a braced top-level block still fires at import; a throw inside a function or arrow body does not.
func TestRegistryHidingBracedTopLevelThrow(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"if.js":    "export const meta = { name: 'x' }\nif (!process.env.X) {\n  throw new Error('x')\n}\n",
		"inl.js":   "export const meta = { name: 'x' }\nif (c) { throw new Error('x') }\n",
		"block.js": "export const meta = { name: 'x' }\n{\n  throw new Error('x')\n}\n",
	} {
		if got := registryHidingDefects(writeScript(t, dir, name, body)); len(got) != 1 || !strings.Contains(got[0], "top-level throw") {
			t.Errorf("%s: got %v, want one top-level throw hit", name, got)
		}
	}
	ok := writeScript(t, dir, "ok.js", "export const meta = { name: 'x' }\nconst f = () => {\n  throw new Error('a')\n}\nfunction g(a, b) {\n  if (a) { throw new Error('b') }\n}\n")
	if got := registryHidingDefects(ok); len(got) != 0 {
		t.Errorf("throws inside callables flagged: %v", got)
	}
}

// An object spread inside the options literal could overwrite an inserted pin, so FillModel leaves it alone.
func TestFillModelObjectSpreadAmbiguous(t *testing.T) {
	src := "agent('p', { ...leafOpts })\n"
	out, ambiguous := FillModel(src, "sonnet")
	if out != src || len(ambiguous) != 1 || ambiguous[0] != "line 1" {
		t.Errorf("out=%q ambiguous=%v; want unchanged and [line 1]", out, ambiguous)
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
