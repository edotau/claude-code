package ask

import (
	"bytes"
	"encoding/json"
	"io"
	"slices"
	"strings"
	"testing"

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
