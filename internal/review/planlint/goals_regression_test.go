package planlint

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestCountPackageMentionsAbsolutePaths(t *testing.T) {
	line := "owns: /home/tester/.claude/internal/a/, /home/tester/.claude/internal/b/, " +
		"/home/tester/.claude/internal/c/, /home/tester/.claude/internal/d/"
	if got := countPackageMentions(line); got != 4 {
		t.Fatalf("countPackageMentions = %d, want 4 distinct packages", got)
	}
	tasks := findOversizedTasks("- [ ] fix x.go " + line + "\n")
	if len(tasks) != 1 || !strings.Contains(tasks[0].Reason, "names 4 packages") {
		t.Fatalf("findOversizedTasks = %v, want one 'names 4 packages' finding", tasks)
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

func TestFinalVerificationUsesStepClassifiers(t *testing.T) {
	for _, tc := range []struct {
		step string
		want bool
	}{
		{"Final: assert response.status == 200", true},
		{"Final: check the response", true},
		{"Final: verify the response", true},
		{"Final: test the response", true},
		{"final", false},
		{"Ship the final artifact", false},
	} {
		t.Run(tc.step, func(t *testing.T) {
			var out, errb bytes.Buffer
			if rc := RunGoals([]string{"-", "--json"}, strings.NewReader("1. "+tc.step+"\n"), &out, &errb); rc != 0 {
				t.Fatalf("RunGoals = %d, stderr=%q", rc, errb.String())
			}
			var result stepsResult
			if err := json.Unmarshal(out.Bytes(), &result); err != nil {
				t.Fatalf("decode goals JSON: %v", err)
			}
			if result.HasFinalVerification != tc.want {
				t.Errorf("has_final_verification = %t, want %t; output=%s", result.HasFinalVerification, tc.want, out.String())
			}
		})
	}
}
