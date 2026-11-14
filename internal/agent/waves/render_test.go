package waves

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

func TestRenderDocumentAndLineBudget(t *testing.T) {
	w := WaveState{N: 1, Status: "failed", Reason: "bad\nreason", Outside: []string{"outside"},
		Violations: []CheckViolation{{Package: "1.1", File: "b"}}, Manual: []string{"1.V"}, Verifier: &stepResult{Cmd: "a\nb", Exit: 1}}
	for i := range 42 {
		w.Leaves = append(w.Leaves, LeafState{ID: strconv.Itoa(i), Status: "error"})
	}
	s := State{Plan: "plan", Base: "base", Waves: []WaveState{w}, Next: "rerun"}
	var out bytes.Buffer
	if err := render(&out, s, false); err != nil {
		t.Fatal(err)
	}
	if strings.Count(out.String(), "\n") > 8 {
		t.Fatal("line budget", &out)
	}
	out.Reset()
	if err := render(&out, s, true); err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"plan", "base", "waves", "next"} {
		if doc[key] == nil {
			t.Fatal("missing", key)
		}
	}
}

func TestLaunchToolsDefaultAndExplicitMode(t *testing.T) {
	no := false
	for _, tc := range []struct {
		options LaunchOptions
		enabled bool
	}{
		{enabled: true}, {options: LaunchOptions{Tools: &no}},
		{options: LaunchOptions{Route: []string{"--no-tools"}}},
		{options: LaunchOptions{Route: []string{"--read-only"}}},
	} {
		_, tools, _, err := LeafLaunch(tc.options, "/tmp/a", "check")
		if err != nil || (tools != nil && *tools) != tc.enabled {
			t.Fatalf("tools %v err %v", tools, err)
		}
	}
}
