package ask

import (
	"bytes"
	"encoding/json"
	"io"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/hosttest"
)

func TestReviewFindingsParseAndDegrade(t *testing.T) {
	rows := `[{"file":"internal/cli/agents.go","line":742,"severity":"HIGH","claim":"The separator is removed before harness parsing."}]`
	for _, tc := range []struct {
		answer, kind string
		code         int
		findings     bool
	}{
		{`{"findings":` + rows + `}`, "", 0, true},
		{`{"findings":[]}`, "", 0, true},
		{"FIX REQUIRED\n[HIGH] internal/cli/agents.go:742 — separator lost\n", "schema", 1, false},
		{`{"findings":[{"file":"x.go","line":"bad","severity":"HIGH","claim":"bad line"}]}`, "schema", 1, false},
	} {
		t.Run(tc.answer, func(t *testing.T) {
			data, err := json.Marshal(map[string]any{"schema": 1, "status": "ok", "agent": "codex", "answer": tc.answer, "dispatch_id": "review-123", "usage": map[string]int{"input_tokens": 23, "output_tokens": 7}})
			if err != nil {
				t.Fatal(err)
			}
			if tc.kind != "" {
				data = bytes.Replace(data, []byte(`"status":"ok"`), []byte(`"error":{"kind":"schema","message":"answer fails schema"},"status":"error"`), 1)
			}
			var note bytes.Buffer
			env, rc, err := parseReviewEnvelope(data, tc.code, &note)
			if err != nil || rc != 0 || env.Error != nil || env.Answer != tc.answer || env.DispatchID != "review-123" || env.Usage.InputTokens != 23 {
				t.Fatalf("env=%+v rc=%d err=%v", env, rc, err)
			}
			if want := strings.TrimPrefix(strings.TrimSuffix(tc.answer, "}"), `{"findings":`); tc.findings && string(env.Findings) != want {
				t.Fatalf("findings = %s", env.Findings)
			}
			if !tc.findings && (len(env.Findings) > 0 || strings.Count(note.String(), "\n") != 1) {
				t.Fatalf("findings=%s note=%q", env.Findings, note.String())
			}
		})
	}
	args := reviewFormatArgs("codex", []string{"--agent", "codex", "review diff"}, true)
	if !slices.Contains(args, "--output-schema") || !slices.Contains(args, reviewFindingsSchema) || args[len(args)-1] != "review diff" {
		t.Fatalf("args=%q", args)
	}
}

func TestReviewTextUnchanged(t *testing.T) {
	original := reviewAskCmd
	t.Cleanup(func() { reviewAskCmd = original })
	want := "FIX REQUIRED\n[HIGH] a.go:7 — lost error — return it\n"
	args := []string{"--agent", "codex", reviewPrompt(false, "errors")}
	reviewAskCmd = func(got []string, out io.Writer) int {
		if !slices.Equal(got, args) {
			t.Fatalf("text argv changed: %q", got)
		}
		if _, err := io.WriteString(out, want); err != nil {
			t.Fatal(err)
		}
		return 0
	}
	rc, out, stderr := hosttest.CaptureOutput(t, func() int { return runReviewAsk("codex", args, ReviewOptions{}) })
	if rc != 0 || out != want || stderr != "" {
		t.Fatalf("rc=%d stdout=%q stderr=%q", rc, out, stderr)
	}
}

func TestReviewFindingsCarriedOnce(t *testing.T) {
	data := []byte(`{"schema":1,"status":"ok","agent":"codex","answer":"{\"findings\":[]}","structured":{"findings":[]}}`)
	env, _, err := parseReviewEnvelope(data, 0, io.Discard)
	if err != nil || string(env.Findings) != "[]" || len(env.Structured) != 0 {
		t.Fatalf("findings=%s structured=%s err=%v", env.Findings, env.Structured, err)
	}
}

func TestSumReviewDenials(t *testing.T) {
	a := sumReviewDenials(nil, &wire.Denials{Count: 2, Commands: []string{"a", "b"}})
	a = sumReviewDenials(a, nil)
	a = sumReviewDenials(a, &wire.Denials{Count: 3, Commands: []string{"c", "d"}})
	if a.Count != 5 || !slices.Equal(a.Commands, []string{"a", "b", "c"}) {
		t.Fatalf("denials=%+v", a)
	}
}

func TestJudgeReportComputesVerdict(t *testing.T) {
	for _, tc := range []struct {
		answer, want string
		mergeable    bool
		high, medium int
	}{
		{"SHIP\n[HIGH] a.go:3 — lost error — return it\n", "FIX REQUIRED", false, 1, 0},
		{"SHIP\n[MEDIUM] a.go:3 — naming\n- [LOW] b.go:9 — typo\n", "FIX REQUIRED", true, 0, 1},
		{"FIX REQUIRED\n[HIGH] no line number here\n", "unjudged", false, 0, 0},
		{"SHIP\n", "SHIP", false, 0, 0},
		{"looks fine to me", "unjudged", false, 0, 0},
	} {
		j := judgeReport(tc.answer)
		t.Logf("%q -> %s counts=%v mergeable=%v note=%q", tc.answer, j.Computed, j.Counts, j.Mergeable, j.Note())
		if j.Computed != tc.want || j.Mergeable != tc.mergeable || j.Counts["HIGH"] != tc.high || j.Counts["MEDIUM"] != tc.medium {
			t.Errorf("%q: %+v", tc.answer, j.reviewVerdict)
		}
	}
	if n := judgeReport("SHIP\n[HIGH] a.go:3 — x\n").Note(); n != "stated SHIP, findings compute FIX REQUIRED" {
		t.Errorf("note = %q", n)
	}
}

func TestStructuredPartKeepsSpacedPathAndMergeableLine(t *testing.T) {
	env := reviewEnvelope{Answer: "SHIP", Findings: json.RawMessage(`[{"file":"my dir/a.go","line":2,"severity":"CRITICAL","claim":"x"}]`)}
	if j := envJudged(env); j.Computed != "BLOCK" || withVerdict(env).VerdictNote != "stated SHIP, findings compute BLOCK" {
		t.Fatalf("structured: %+v", j.reviewVerdict)
	}
	m := mergeJudged([]judged{envJudged(env)}, false)
	if m.Computed != "BLOCK" {
		t.Fatalf("merged: %+v", m.reviewVerdict)
	}
	out := mergeReports(false, []part{{}}, []string{"SHIP\n[LOW] a.go:1 — x\n"})
	if !strings.HasPrefix(out, "FIX REQUIRED\nmergeable: only MEDIUM/LOW findings, fix on main\n") {
		t.Fatalf("mergeable line:\n%s", out)
	}
}

func TestPartProseKeepsAnswerForEmptyFindings(t *testing.T) {
	env := reviewEnvelope{Answer: cleanVerdict, Findings: json.RawMessage(`[]`)}
	if got := partProse(env); got != cleanVerdict || envJudged(env).Computed != "SHIP" {
		t.Fatalf("prose=%q judged=%+v", got, envJudged(env).reviewVerdict)
	}
}

func TestSchemaInvalidFindingsAreUnjudged(t *testing.T) {
	env, err := reviewFindings(reviewEnvelope{Structured: json.RawMessage(`{"findings":"x"}`)}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	env.Answer = "SHIP\n"
	if v := withVerdict(env).Computed; v != "unjudged" {
		t.Fatalf("computed = %q", v)
	}
}

func TestGeneratedSchemasMatchContract(t *testing.T) {
	for name, tc := range map[string]struct{ got, want string }{
		"review": {reviewFindingsSchema, `{"type":"object","additionalProperties":false,"required":["findings"],"properties":{"findings":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["file","line","severity","claim"],"properties":{"file":{"type":"string"},"line":{"type":"integer"},"severity":{"type":"string","enum":["CRITICAL","HIGH","MEDIUM","LOW"]},"claim":{"type":"string"}}}}}}`},
		"loop":   {loopPlanSchema, `{"type":"object","additionalProperties":false,"required":["done"],"properties":{"done":{"type":"boolean"},"summary":{"type":"string"},"parts":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["id","prompt"],"properties":{"id":{"type":"string"},"prompt":{"type":"string"},"agent":{"type":"string"},"model":{"type":"string"},"owns":{"type":"array","items":{"type":"string"}},"timeout":{"type":"string"}}}}}}`},
	} {
		var a, b any
		if json.Unmarshal([]byte(tc.got), &a) != nil || json.Unmarshal([]byte(tc.want), &b) != nil || !reflect.DeepEqual(a, b) {
			t.Errorf("%s schema differs:\n%s", name, tc.got)
		}
	}
	if raw, _, err := wire.Structured(`{"findings":[],"extra":1}`, reviewFindingsSchema); err != nil || string(raw) != `{"findings":[]}` {
		t.Errorf("extra key not dropped: %s %v", raw, err)
	}
}

func TestExtraKeysDroppedNotFatal(t *testing.T) {
	raw, notes, err := wire.Structured(`{"done":true,"summary":"s","parts":[],"notes":"x"}`, loopPlanSchema)
	var p loopPlan
	if err != nil || json.Unmarshal(raw, &p) != nil || !p.Done || !slices.Contains(notes, "dropped answer.notes") {
		t.Fatalf("plan=%+v notes=%v err=%v", p, notes, err)
	}
	row := `{"file":"a.go","line":3,"severity":"HIGH","claim":"c","why":"x"}`
	env, err := reviewFindings(reviewEnvelope{Structured: json.RawMessage(`{"findings":[` + row + `],"notes":"x"}`)}, io.Discard)
	if err != nil || env.Computed == "unjudged" || !strings.Contains(string(env.Findings), `"claim":"c"`) {
		t.Fatalf("findings=%s computed=%q err=%v", env.Findings, env.Computed, err)
	}
}

func TestParseLoopPlanLenient(t *testing.T) {
	p, _, err := wire.Decode[loopPlan](`{"done":true}`)
	if err != nil || !p.Done {
		t.Fatalf("done only: %v %v", p, err)
	}
	p, notes, err := wire.Decode[loopPlan](`{"done":false,"parts":[{"id":"a","prompt":"p","agent":null}]}`)
	if err != nil || len(p.Parts) != 1 || len(notes) != 1 {
		t.Fatalf("null optional: %v %v %v", p, notes, err)
	}
	if _, _, err = wire.Decode[loopPlan](`{"done":null}`); err == nil {
		t.Fatal("null required must error")
	}
}
