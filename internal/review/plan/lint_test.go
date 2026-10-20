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
const c = await agent('task three', { agentType: 'code-worker' })
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

// FillModel: each case covers one shape the rewrite must handle (insert, empty object, one-arg
// append, already pinned, non-object options) or correctly refuse (identifier/spread/ternary options).
func TestFillModel(t *testing.T) {
	const model = "sonnet"

	t.Run("label-only object gets model inserted, existing key survives", func(t *testing.T) {
		out, ambiguous := FillModel("agent('p', { label: 'l' })\n", model)
		if !strings.Contains(out, "model: 'sonnet',") {
			t.Errorf("out = %q, want an inserted model: 'sonnet',", out)
		}
		if !strings.Contains(out, "label: 'l'") {
			t.Errorf("out = %q, want the original label: 'l' to survive", out)
		}
		if len(ambiguous) != 0 {
			t.Errorf("ambiguous = %v, want none", ambiguous)
		}
	})

	t.Run("empty object gets model with no stray comma", func(t *testing.T) {
		out, ambiguous := FillModel("agent('p', {})\n", model)
		if stripped := strings.ReplaceAll(out, " ", ""); !strings.Contains(stripped, "{model:'sonnet'}") {
			t.Errorf("out = %q, want {model: 'sonnet'} (any spacing)", out)
		}
		if len(ambiguous) != 0 {
			t.Errorf("ambiguous = %v, want none", ambiguous)
		}
	})

	t.Run("one-arg call gets an options object appended", func(t *testing.T) {
		out, ambiguous := FillModel("agent('p')\n", model)
		if !strings.Contains(out, ", { model: 'sonnet' }") {
			t.Errorf("out = %q, want an appended , { model: 'sonnet' }", out)
		}
		if len(ambiguous) != 0 {
			t.Errorf("ambiguous = %v, want none", ambiguous)
		}
	})

	t.Run("already pinned calls are left unchanged", func(t *testing.T) {
		for _, src := range []string{
			"agent('p', { model: 'opus' })\n",
			"agent('p', { agentType: 'code-worker', model: 'sonnet' })\n",
		} {
			out, ambiguous := FillModel(src, model)
			if out != src {
				t.Errorf("src = %q: out = %q, want unchanged", src, out)
			}
			if len(ambiguous) != 0 {
				t.Errorf("src = %q: ambiguous = %v, want none", src, ambiguous)
			}
		}
	})

	t.Run("options as a bare identifier is ambiguous, unchanged", func(t *testing.T) {
		src := "agent('p', opts)\n"
		out, ambiguous := FillModel(src, model)
		if out != src {
			t.Errorf("out = %q, want unchanged", out)
		}
		if len(ambiguous) != 1 || ambiguous[0] != "line 1" {
			t.Errorf("ambiguous = %v, want exactly [\"line 1\"]", ambiguous)
		}
	})

	t.Run("spread or ternary options are ambiguous, unchanged", func(t *testing.T) {
		for _, src := range []string{
			"agent('p', ...opts)\n",
			"agent('p', cond ? a : b)\n",
		} {
			out, ambiguous := FillModel(src, model)
			if out != src {
				t.Errorf("src = %q: out = %q, want unchanged", src, out)
			}
			if len(ambiguous) != 1 || ambiguous[0] != "line 1" {
				t.Errorf("src = %q: ambiguous = %v, want exactly [\"line 1\"]", src, ambiguous)
			}
		}
	})

	t.Run("two calls on different lines both filled with correct back-to-front offsets", func(t *testing.T) {
		src := "agent('a', { label: 'x' })\nagent('b', { label: 'y' })\n"
		out, ambiguous := FillModel(src, model)
		if len(ambiguous) != 0 {
			t.Fatalf("ambiguous = %v, want none", ambiguous)
		}
		if n := strings.Count(out, "model: 'sonnet',"); n != 2 {
			t.Errorf("out = %q, want 2 insertions, got %d", out, n)
		}
		if strings.Count(out, "\n") != strings.Count(src, "\n") {
			t.Errorf("out = %q changed the line count", out)
		}
	})

	t.Run("a template literal mentioning agent( and model: is neither a call nor a pin", func(t *testing.T) {
		src := "const prompt = `call agent(x) with model: sonnet`\nagent('p', { label: 'l' })\n"
		out, ambiguous := FillModel(src, model)
		if len(ambiguous) != 0 {
			t.Fatalf("ambiguous = %v, want none (the template text is not a call)", ambiguous)
		}
		if n := strings.Count(out, "model: 'sonnet',"); n != 1 {
			t.Errorf("out = %q, want exactly 1 real insertion, got %d", out, n)
		}
		if !strings.Contains(out, "call agent(x) with model: sonnet") {
			t.Errorf("out = %q, template literal text must survive untouched", out)
		}
	})
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

func readScript(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
