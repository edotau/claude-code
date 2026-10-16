package plan

import (
	"bytes"
	"os"
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
	hits := effortlessAgentCalls(p)
	if len(hits) != 1 || hits[0] != p+":2" {
		t.Fatalf("hits = %v, want exactly the line-2 call (a model pin does not imply effort)", hits)
	}
}

// Unpinned agent() is flagged; model:/agentType:/opus-ok/word-prefixed calls are skipped.
func TestUnpinnedAgentCalls(t *testing.T) {
	p := writeScript(t, t.TempDir(), "wf.js", `const a = await agent('task one', { label: 'x', schema: S })
const b = await agent('task two', { label: 'y', model: 'sonnet' })
const c = await agent('task three', { agentType: 'code-workers' })
const d = await agent('judge', { label: 'j' }) // lint:opus-ok
const e = await subagent('not the primitive')
`)
	hits := unpinnedAgentCalls(p)
	if len(hits) != 1 || hits[0] != p+":1" {
		t.Fatalf("hits = %v, want exactly the line-1 unpinned call", hits)
	}
}

// A prompt string containing "effort:"/"model:" text must not satisfy the pin check, and a paren
// inside the prompt must not end the call window before the real options object.
func TestStringLiteralDoesNotMaskPin(t *testing.T) {
	p := writeScript(t, t.TempDir(), "wf.js", "const a = await agent('task', { prompt: \"remember effort: low later, model: sonnet too\" })\n"+
		"const b = await agent(`run f(x) then ) report`, { model: 'sonnet', effort: 'low' })\n")
	if hits := effortlessAgentCalls(p); len(hits) != 1 || hits[0] != p+":1" {
		t.Fatalf("effortlessAgentCalls = %v, want only line 1", hits)
	}
	if hits := unpinnedAgentCalls(p); len(hits) != 1 || hits[0] != p+":1" {
		t.Fatalf("unpinnedAgentCalls = %v, want only line 1", hits)
	}
}

// A read error must surface as a hit, not silently report the file clean.
func TestUnreadableFileIsAHit(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.js")
	for name, fn := range map[string]func(string) []string{"unpinned": unpinnedAgentCalls, "effortless": effortlessAgentCalls, "registry": registryHidingDefects} {
		if hits := fn(missing); len(hits) != 1 || !strings.Contains(hits[0], "cannot read file") {
			t.Errorf("%s(missing) = %v, want one 'cannot read file' hit", name, hits)
		}
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

// The shipped scripts must satisfy every check — a lint that lands red breaks make audit everywhere.
// Anchored on this source file's path so a cwd change can never make the guard skip instead of gate.
func TestShippedWorkflowsAreClean(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed — cannot locate the repo root")
	}
	repoRoot := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))) // internal/review/plan/x_test.go -> repo
	var out, errb bytes.Buffer
	if rc := RunWorkflowLint([]string{repoRoot}, &out, &errb); rc != 0 || !strings.Contains(out.String(), "script(s) clean") || strings.HasPrefix(out.String(), "workflow lint: 0 ") {
		t.Fatalf("shipped workflows under %s: rc=%d out=%q\n%s", repoRoot, rc, out.String(), errb.String())
	}
}
