package planlint

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

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
