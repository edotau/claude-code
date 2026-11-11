package quality

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Lexer corners (comments, division, nested objects, prompt text) must neither hide a call nor fake its pins.
func TestLintPinsFlagsUnpinnedCall(t *testing.T) {
	unpinned := []string{"agent() without model:", "agent() without effort:"}
	cases := []struct {
		name, src string
		want      []string
	}{
		{"label only", "await agent('x', { label: 'l' })\n", unpinned},
		{"block comment before call", "/* note */ await agent('x') // explanation\n", unpinned},
		{"identifier division", "const x = value / 2; await agent('p') / 2;", unpinned},
		{"number division", "const x = 42 / 2; await agent('p') / 2;", unpinned},
		{"call division", "const x = initialize() / 2; await agent('p') / 2;", unpinned},
		{"array division", "const x = [] / 2; await agent('p') / 2;", unpinned},
		{"object division", "const x = {} / 2; await agent('p') / 2;", unpinned},
		{"postfix increment division", "const x = value++ / 2; await agent('p') / 2;", unpinned},
		{"postfix decrement division", "const x = value-- / 2; await agent('p') / 2;", unpinned},
		{"identifier named of", "const of = 4; of / 2; await agent('p') / 2;", unpinned},
		{"keys inside the prompt argument", "agent(JSON.stringify({model:'sonnet', effort:'low'}))", unpinned},
		{"options inside an array", "agent('p', [{ model: 'gpt-5', effort: 'med' }])", unpinned},
		{"options passed to a helper", "agent('p', makeOptions({ model: 'gpt-5', effort: 'med' }))", unpinned},
		{"fully pinned", "await agent('x', { model: 'sonnet', effort: 'low' })\n", nil},
		{"parenthesized options", "agent('p', ({model:'sonnet',effort:'low'}))", nil},
		{"doubly parenthesized options", "agent('p', (({model:'sonnet',effort:'low'})))", nil},
		{"agent() text inside a string", "export const meta = { description: 'an unpinned agent() probe' }\n" +
			"await agent('x', { model: 'sonnet', effort: 'low' })\n", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hits := LintPins("inline", []byte(tc.src))
			if len(hits) != len(tc.want) {
				t.Fatalf("LintPins(%q) = %v, want %d diagnostics %v", tc.src, hits, len(tc.want), tc.want)
			}
			for i, w := range tc.want {
				if !strings.HasPrefix(hits[i], "inline:1: "+w) {
					t.Errorf("diagnostic %d = %q, want prefix %q", i, hits[i], "inline:1: "+w)
				}
			}
		})
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

func TestQuotedModelKeyIsValidated(t *testing.T) {
	hits := LintPins("inline", []byte("agent('work', { 'model': 'bogus', effort: 'low' })"))
	if len(hits) != 1 || !strings.Contains(hits[0], `model value "bogus"`) {
		t.Fatalf("LintPins = %v, want one invalid-model diagnostic for 'bogus'", hits)
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

type diskFullWriter struct{}

func (diskFullWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestLintFailedSuccessWriteExitsNonZero(t *testing.T) {
	noNode(t)
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
