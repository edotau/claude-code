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
	if strings.Count(out.String(), "\n") > 42+8 { // a row per leaf, plus the verdict and its findings
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

func TestRenderRowPerPackageWithReason(t *testing.T) {
	long := strings.Repeat("go test ./very/long/package/path ", 8)
	w := WaveState{N: 1, Status: "failed", Leaves: []LeafState{
		{ID: "1.1", Agent: "opencode", Status: "ok", Verdict: "ok", ElapsedMs: 17_000, Scoped: true},
		{ID: "1.2", Agent: "opencode", DispatchID: "j2", Status: "ok", Verdict: "failed", Verify: &stepResult{Cmd: long, Exit: 1}, Scoped: true},
		{ID: "1.3", Agent: "codex", DispatchID: "j3", Status: "ok", Verdict: "failed", Scoped: false},
	}, Violations: []CheckViolation{{Package: "1.3", File: "pkg/b.go", Owners: []string{"1.2"}}}}
	var out bytes.Buffer
	if err := render(&out, State{Base: "base", Waves: []WaveState{w}}, false); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	for i, want := range []string{"1.1", "1.2", "1.3"} {
		if !strings.HasPrefix(strings.TrimSpace(lines[i]), want) || len([]rune(lines[i])) > 100 {
			t.Errorf("row %d %q: want id %s within 100 columns", i, lines[i], want)
		}
	}
	for i, want := range []string{"ok", "FAIL", "FAIL"} {
		if !strings.Contains(lines[i], " "+want+" ") {
			t.Errorf("row %d %q: want mark %s", i, lines[i], want)
		}
	}
	if !strings.Contains(lines[0], "17s") || !strings.Contains(lines[1], "verify exit 1: go test") ||
		!strings.Contains(lines[2], "1.3 wrote pkg/b.go (owner 1.2)") || !strings.HasPrefix(lines[3], "wave 1 failed") {
		t.Fatalf("rows or verdict line wrong:\n%s", &out)
	}
	if !strings.Contains(out.String(), "scope: 1.3 cannot refuse a foreign write") {
		t.Fatalf("missing detection-only notice:\n%s", &out)
	}
}

func TestRenderNamesLeavesJudgedWithoutAWriteRecord(t *testing.T) {
	w := WaveState{N: 1, Status: "passed", Fallback: []string{"1.2", "1.4"}}
	var out bytes.Buffer
	if err := render(&out, State{Base: "base", Waves: []WaveState{w}}, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "no write record: 1.2,1.4 (judged by run window, write time)") {
		t.Fatalf("missing fallback notice:\n%s", &out)
	}
}
