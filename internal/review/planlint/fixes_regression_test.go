package planlint

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCountPackageMentionsAbsolutePaths(t *testing.T) {
	line := "owns: /home/edotau/.claude/internal/a/, /home/edotau/.claude/internal/b/, " +
		"/home/edotau/.claude/internal/c/, /home/edotau/.claude/internal/d/"
	if got := countPackageMentions(line); got != 4 {
		t.Fatalf("countPackageMentions = %d, want 4 distinct packages", got)
	}
	tasks := findOversizedTasks("- [ ] fix x.go " + line + "\n")
	if len(tasks) != 1 || !strings.Contains(tasks[0].Reason, "names 4 packages") {
		t.Fatalf("findOversizedTasks = %v, want one 'names 4 packages' finding", tasks)
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
	src = "agent('p', /* a, b */ { effort: 'low' })" // a leading comment is conservatively ambiguous, never mangled
	if out, amb := FillModel(src, "sonnet"); out != src || len(amb) != 1 {
		t.Errorf("FillModel (comment before options) = %q, %v; want unchanged and one ambiguous line", out, amb)
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

func TestContinuationLineVerificationCounts(t *testing.T) {
	missing := func(text string) int {
		n := 0
		for _, f := range lintText(text) {
			if f.Category == "missing-verification" {
				n++
			}
		}
		return n
	}
	if got := missing("1. Create the endpoint.\n   verify: check HTTP status 200.\n"); got != 0 {
		t.Errorf("indented verify line: %d missing-verification findings, want 0", got)
	}
	if got := missing("1. Create the endpoint.\n   Then wire it.\n"); got != 1 {
		t.Errorf("no verification: %d missing-verification findings, want 1", got)
	}
}

func TestShorterFenceDoesNotCloseLongerFence(t *testing.T) {
	var f fenceState
	for _, line := range []string{"````", "```", "1. inside"} {
		if !f.skip(line) {
			t.Fatalf("skip(%q) = false, want true while the 4-backtick fence is open", line)
		}
	}
	if f.n != 4 {
		t.Fatalf("fence length = %d after the ``` line, want 4 (still open)", f.n)
	}
	if !f.skip("````") || f.n != 0 {
		t.Errorf("a 4-backtick line must close the fence, state %+v", f)
	}
}

func TestFinalWordAloneIsNotFinalVerification(t *testing.T) {
	if a := analyze("1. Create it.\n2. Ship the final artifact.\n", "x"); a.hasFinalVerification {
		t.Errorf("'Ship the final artifact' set hasFinalVerification")
	}
	if a := analyze("1. Create it.\n2. Run the final regression tests.\n", "x"); !a.hasFinalVerification {
		t.Errorf("'Run the final regression tests' did not set hasFinalVerification")
	}
}

func TestSkeletonEscapesWorkflowName(t *testing.T) {
	var out, errb bytes.Buffer
	if rc := RunSkeleton([]string{"--name", "team's-flow", "pipeline"}, &out, &errb); rc != 0 {
		t.Fatalf("RunSkeleton rc=%d err=%q", rc, errb.String())
	}
	if !strings.Contains(out.String(), `name: 'team\'s-flow',`) {
		t.Errorf("skeleton meta = %q, want name: 'team\\'s-flow'", out.String())
	}
	if got := metaBlock("a\\b\nc", "d", ""); !strings.Contains(got, `name: 'a\\b\nc',`) {
		t.Errorf("metaBlock backslash/newline = %q", got)
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
