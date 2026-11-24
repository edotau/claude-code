package hooks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/proc"
)

func TestReviewHandback(t *testing.T) {
	for _, c := range []struct {
		name, report string
		want         bool
	}{
		{"left as-is", "LOW: naming. Left as-is.", true},
		{"left as is", "left as is", true},
		{"not fixed", "the race was not fixed", true},
		{"owner", "Decisions for the owner: pick A", true},
		{"want me to", "Want me to also rename it?", true},
		{"say yes", "say yes and I'll merge", true},
		{"severity open", "MEDIUM finding still open", true},
		{"remaining severity", "remaining: HIGH in api.go", true},
		{"clean", "DONE. No CRITICAL, HIGH or MEDIUM remaining; gates green.", false},
		{"plain", "DONE: fixed all findings, gates green.", false},
		{"lowercase severity", "the open file is high priority", false},
	} {
		in := core.ParseInput(strings.NewReader(`{"tool_name":"Agent","tool_response":{"content":[{"type":"text","text":` +
			jsonStr(c.report) + `}],"prompt":"want me to"}}`))
		r := ReviewHandback(in)
		if got := r.Context != ""; got != c.want || r.Decision != core.Allow {
			t.Errorf("%s: context %q decision %v, want hit=%v and no block", c.name, r.Context, r.Decision, c.want)
		}
		if c.want && !strings.HasPrefix(r.Context, "Fix these now; do not relay them to the owner as questions") {
			t.Errorf("%s: context %q lacks the instruction", c.name, r.Context)
		}
	}
}

func jsonStr(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s) + `"`
}

func TestReviewDecide(t *testing.T) {
	text := func(s string) string {
		return `{"type":"assistant","message":{"content":[{"type":"text","text":` + jsonStr(s) + `}]}}`
	}
	user := `{"type":"user","message":{"content":"go"}}`
	result := `{"type":"user","message":{"content":[{"type":"tool_result","content":"ok"}]}}`
	ask := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"AskUserQuestion","input":{"questions":[{"question":"Fix the LOW findings?","options":[{"label":"Fix"},{"label":"Leave"}]}]}}]}}`
	for _, c := range []struct {
		name  string
		lines []string
		stop  bool
		want  bool
	}{
		{"want me to fix", []string{user, text("Done.\nWant me to fix the LOW findings?")}, false, true},
		{"your call follow-up", []string{user, text("Your call on the follow-up.")}, false, true},
		{"ask question", []string{user, ask}, false, true},
		{"ask before tool result", []string{user, ask, result, text("ok")}, false, true},
		{"already blocked", []string{user, text("Want me to fix it?")}, true, false},
		{"plain report", []string{user, text("DONE. Fixed 3 findings; gates green.")}, false, false},
		{"requirement question", []string{user, text("Two readings: A or B. Which do you want?")}, false, false},
		{"old turn", []string{text("Want me to fix it?"), user, text("DONE.")}, false, false},
		{"no transcript", nil, false, false},
	} {
		p := filepath.Join(t.TempDir(), "t.jsonl")
		if c.lines != nil {
			if err := os.WriteFile(p, []byte(strings.Join(c.lines, "\n")+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		stop := "false"
		if c.stop {
			stop = "true"
		}
		in := core.ParseInput(strings.NewReader(`{"hook_event_name":"Stop","stop_hook_active":` + stop + `,"transcript_path":` + jsonStr(p) + `}`))
		if r := ReviewDecide(in); (r.Decision == core.Block) != c.want || (c.want && !strings.Contains(r.Reason, "Decide and act")) {
			t.Errorf("%s: got %+v, want block=%v", c.name, r, c.want)
		}
	}
	p := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(p, []byte(text("Want me to fix it?")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(proc.DispatchIDEnv, "d1")
	if r := ReviewDecide(core.ParseInput(strings.NewReader(`{"transcript_path":` + jsonStr(p) + `}`))); r.Decision == core.Block {
		t.Error("a dispatched agent was blocked")
	}
}
