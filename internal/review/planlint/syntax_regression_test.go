package planlint

import (
	"strings"
	"testing"
)

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
