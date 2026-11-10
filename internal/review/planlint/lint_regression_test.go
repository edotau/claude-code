package planlint

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBlockCommentTerminatorDoesNotHideAgentCall(t *testing.T) {
	src := "/* note */ await agent('x') // explanation\n"
	hits := LintPins("inline", []byte(src))
	if len(hits) != 2 {
		t.Fatalf("LintPins = %v, want missing model and effort for the call after the block comment", hits)
	}
	if !strings.HasPrefix(hits[0], "inline:1: agent() without model:") {
		t.Errorf("model diagnostic = %q, want missing model on line 1", hits[0])
	}
	if !strings.HasPrefix(hits[1], "inline:1: agent() without effort:") {
		t.Errorf("effort diagnostic = %q, want missing effort on line 1", hits[1])
	}
}

func TestForAwaitDoesNotHideModuleThrow(t *testing.T) {
	src := "export const meta = { name: 'x' }\nfor await (const item of items) {\n  throw new Error('import')\n}\n"
	p := writeScript(t, t.TempDir(), "for-await.js", src)
	hits := registryHidingDefects(p)
	if len(hits) != 1 {
		t.Fatalf("registryHidingDefects = %v, want one module-level throw inside for await", hits)
	}
	if !strings.HasPrefix(hits[0], p+":3:") || !strings.Contains(hits[0], "top-level throw") {
		t.Errorf("diagnostic = %q, want top-level throw on line 3", hits[0])
	}
}

func TestTernaryLabelDoesNotPinModel(t *testing.T) {
	src := "agent('x', { label: flag ? 'model' : 'sonnet', effort: 'low' })"
	hits := LintPins("inline", []byte(src))
	if len(hits) != 1 {
		t.Fatalf("LintPins = %v, want one missing-model diagnostic", hits)
	}
	if !strings.HasPrefix(hits[0], "inline:1: agent() without model:") {
		t.Errorf("diagnostic = %q, want missing model on line 1", hits[0])
	}
	out, ambiguous := FillModel(src, "sonnet")
	want := "agent('x', { model: 'sonnet', label: flag ? 'model' : 'sonnet', effort: 'low' })"
	if out != want || len(ambiguous) != 0 {
		t.Fatalf("FillModel = %q, %v; want %q, no ambiguity", out, ambiguous, want)
	}
	if remaining := LintPins("inline", []byte(out)); len(remaining) != 0 {
		t.Errorf("LintPins after FillModel = %v, want no diagnostics", remaining)
	}
}

func TestDivisionDoesNotHideUnpinnedAgent(t *testing.T) {
	for _, expr := range []string{"value", "42", "initialize()", "[]", "{}", "value++", "value--"} {
		t.Run(expr, func(t *testing.T) {
			src := "const x = " + expr + " / 2; await agent('p') / 2;"
			hits := LintPins("division.js", []byte(src))
			if len(hits) != 2 {
				t.Fatalf("LintPins = %v, want missing model and effort after division", hits)
			}
			if !strings.HasPrefix(hits[0], "division.js:1: agent() without model:") {
				t.Errorf("model diagnostic = %q, want missing model on line 1", hits[0])
			}
			if !strings.HasPrefix(hits[1], "division.js:1: agent() without effort:") {
				t.Errorf("effort diagnostic = %q, want missing effort on line 1", hits[1])
			}
		})
	}
}

func TestModuleBlockAfterCallReportsThrow(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		want int
	}{
		{"module", "initialize()\n{ throw new Error('import') }", 1},
		{"nested module", "{ initialize()\n{ throw new Error('import') } }", 1},
		{"object method", "const obj = { initialize() { throw new Error('deferred') } }", 0},
		{"class method", "class Worker { initialize() { throw new Error('deferred') } }", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := writeScript(t, t.TempDir(), "block.js", "export const meta = { name: 'block' }\n"+tc.src+"\n")
			hits := registryHidingDefects(p)
			if len(hits) != tc.want {
				t.Fatalf("registryHidingDefects = %v, want %d module-level throws", hits, tc.want)
			}
			if tc.want == 1 && !strings.HasPrefix(hits[0], p+":3: top-level throw") {
				t.Errorf("diagnostic = %q, want top-level throw on line 3", hits[0])
			}
		})
	}
}

func TestOfIdentifierDivisionReportsUnpinnedAgent(t *testing.T) {
	src := "const of = 4; of / 2; await agent('p') / 2;"
	hits := LintPins("of.js", []byte(src))
	if len(hits) != 2 {
		t.Fatalf("LintPins = %v, want missing model and effort after division by identifier of", hits)
	}
	if !strings.HasPrefix(hits[0], "of.js:1: agent() without model:") {
		t.Errorf("model diagnostic = %q, want missing model on line 1", hits[0])
	}
	if !strings.HasPrefix(hits[1], "of.js:1: agent() without effort:") {
		t.Errorf("effort diagnostic = %q, want missing effort on line 1", hits[1])
	}
}

func TestParenthesizedOptionsRetainPins(t *testing.T) {
	for _, src := range []string{
		"agent('p', ({model:'sonnet',effort:'low'}))",
		"agent('p', (({model:'sonnet',effort:'low'})))",
	} {
		if hits := LintPins("grouped.js", []byte(src)); len(hits) != 0 {
			t.Errorf("LintPins(%q) = %v, want no diagnostics for parenthesized pinned options", src, hits)
		}
	}
}

func TestLabeledBlockAfterCallReportsModuleThrow(t *testing.T) {
	src := "export const meta = { name: 'labeled' }\nlabel: { initialize()\n{ throw Error('import') } }\n"
	path := writeScript(t, t.TempDir(), "labeled.js", src)
	hits := registryHidingDefects(path)
	if len(hits) != 1 {
		t.Fatalf("registryHidingDefects = %v, want one module-level throw inside the labeled block", hits)
	}
	if !strings.HasPrefix(hits[0], path+":3: top-level throw") {
		t.Errorf("diagnostic = %q, want top-level throw on line 3", hits[0])
	}
}

func TestScanAgentCallsWhitespaceBeforeParen(t *testing.T) {
	for _, src := range []string{"await agent ('work')", "await agent\n('work')", "await agent\t('work')"} {
		if hits := LintPins("inline", []byte(src)); len(hits) != 2 {
			t.Errorf("LintPins(%q) = %v, want missing model and effort", src, hits)
		}
	}
	if hits := LintPins("inline", []byte("agentic ('work')")); len(hits) != 0 {
		t.Errorf("agentic (...) = %v, want no diagnostics", hits)
	}
}

func TestKeysInsidePromptArgumentAreNotOptions(t *testing.T) {
	src := "agent(JSON.stringify({model:'sonnet', effort:'low'}))"
	hits := LintPins("inline", []byte(src))
	if len(hits) != 2 {
		t.Fatalf("LintPins = %v, want missing model and effort", hits)
	}
}

func TestQuotedModelKeyIsValidated(t *testing.T) {
	hits := LintPins("inline", []byte("agent('work', { 'model': 'bogus', effort: 'low' })"))
	if len(hits) != 1 || !strings.Contains(hits[0], `model value "bogus"`) {
		t.Fatalf("LintPins = %v, want one invalid-model diagnostic for 'bogus'", hits)
	}
}

func TestFillModelSkipsRegexLiteral(t *testing.T) {
	src := "const re = /agent(foo)/;"
	out, ambiguous := FillModel(src, "sonnet")
	if out != src || len(ambiguous) != 0 {
		t.Fatalf("FillModel = %q, %v; want the regex untouched and no ambiguity", out, ambiguous)
	}
}

func TestCommentCommaDoesNotSplitArgs(t *testing.T) {
	s := "'p', /* a, b */ { effort: 'low' } // c, d"
	if spans := splitTopLevelArgs(s); len(spans) != 2 {
		t.Fatalf("splitTopLevelArgs(%q) = %v, want 2 arguments", s, spans)
	}
	src := "agent('p', { effort: 'low' } /* a, b */)"
	want := "agent('p', { model: 'sonnet', effort: 'low' } /* a, b */)"
	if out, amb := FillModel(src, "sonnet"); out != want || len(amb) != 0 {
		t.Errorf("FillModel = %q, %v; want %q", out, amb, want)
	}
	src = "agent('p', /* a, b */ { effort: 'low' })" // a leading comment is skipped, the object still gets the pin
	want = "agent('p', /* a, b */ { model: 'sonnet', effort: 'low' })"
	if out, amb := FillModel(src, "sonnet"); out != want || len(amb) != 0 {
		t.Errorf("FillModel (comment before options) = %q, %v; want %q", out, amb, want)
	}
	src = "agent('p', /* a */ opts)" // a non-literal after the comment stays ambiguous
	if out, amb := FillModel(src, "sonnet"); out != src || len(amb) != 1 {
		t.Errorf("FillModel (comment before identifier) = %q, %v; want unchanged and one ambiguous line", out, amb)
	}
}

func TestMethodThrowsAreNotTopLevel(t *testing.T) {
	cases := map[string]int{
		"const helper = { run() { throw new Error('x') } }\n": 0,
		"class W { run() { throw new Error('y') } }\n":        0,
		"if (ok) {\n  throw new Error('z')\n}\n":              1,
	}
	for src, want := range cases {
		if got := topLevelThrowLines(stripStrings(src)); len(got) != want {
			t.Errorf("topLevelThrowLines(%q) = %v, want %d", src, got, want)
		}
	}
}

type diskFullWriter struct{}

func (diskFullWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestLintFailedSuccessWriteExitsNonZero(t *testing.T) {
	useNode(t)
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeScript(t, filepath.Join(root, "workflows"), "ok.js", "export const meta = { name: 'ok' }\nawait agent('x', { model: 'sonnet', effort: 'low' })\n")
	var errb bytes.Buffer
	if rc := RunWorkflowLint([]string{root}, diskFullWriter{}, &errb); rc == 0 || !strings.Contains(errb.String(), "writing output") {
		t.Fatalf("rc=%d stderr=%q, want non-zero with a 'writing output' message", rc, errb.String())
	}
}
