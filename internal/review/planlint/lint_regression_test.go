package planlint

import (
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
