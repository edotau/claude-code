package hooks

import (
	"os"
	"path/filepath"
	"slices"
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
		{"negation scoped to its clause", "No HIGH remaining; 2 MEDIUM remaining in api.go", true},
		{"zero-length is no negation", "HIGH remaining: zero-length input panics", true},
		{"clean no findings", "No findings left as-is; all were fixed", false},
		{"left it covered", "Fixed the race and left it covered by the regression test.", false},
		{"deferred", "MEDIUM: parser leak deferred to a follow-up", true},
		{"numbered finding", "1. MEDIUM - parser leaks on empty input", true},
		{"numbered fixed", "1. MEDIUM - parser leak, fixed in a.go", false},
		{"negation then new clause", "No HIGH remaining, but 2 MEDIUM remaining", true},
		{"not all fixed", "Not all fixed: MEDIUM remaining", true},
		{"fixed-size item", "1. MEDIUM - fixed-size buffer overflows on long names", true},
		{"tested but broken", "MEDIUM parser left as-is, tested but still broken", true},
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

func transcriptText(s string) string {
	return `{"type":"assistant","message":{"content":[{"type":"text","text":` + jsonStr(s) + `}]}}`
}

func transcriptAsk(question, option string) string {
	return `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"AskUserQuestion","input":{"questions":[` +
		`{"question":"` + question + `","options":[{"label":"Yes","description":"` + option + `"}]}]}}]}}`
}

// stopResult runs ReviewDecide on a Stop payload over a transcript of lines.
func stopResult(t *testing.T, lines []string, stopActive bool) core.Result {
	t.Helper()
	p := filepath.Join(t.TempDir(), "t.jsonl")
	if lines != nil {
		if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	active := "false"
	if stopActive {
		active = "true"
	}
	return ReviewDecide(core.ParseInput(strings.NewReader(`{"hook_event_name":"Stop","stop_hook_active":` + active +
		`,"transcript_path":` + jsonStr(p) + `,"last_assistant_message":` + jsonStr(lastText(lines)) + `}`)))
}

func TestReviewDecide(t *testing.T) {
	text := transcriptText
	user := `{"type":"user","message":{"content":"go"}}`
	result := `{"type":"user","message":{"content":[{"type":"tool_result","content":"ok"}]}}`
	ask := transcriptAsk("Fix the LOW findings?", "")
	meta := `{"type":"user","isMeta":true,"message":{"content":"<task-notification>done</task-notification>"}}`
	twoBlocks := `{"type":"assistant","message":{"content":[{"type":"text","text":"All checks pass."},` +
		`{"type":"text","text":"Want me to fix the LOW findings?"}]}}`
	askID := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"a1","name":"AskUserQuestion","input":{"questions":[` +
		`{"question":"Fix the LOW findings?"}]}}]}}`
	otherUse := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"b2","name":"Bash","input":{}}]}}`
	otherResult := `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"b2","content":"ok"}]}}`
	huge := text(strings.Repeat("x", turnTailBytes+10) + "\nWant me to fix the LOW findings?") // last_assistant_message backs it up
	for _, c := range []struct {
		name  string
		lines []string
		stop  bool
		want  bool
	}{
		{"want me to fix", []string{user, text("Done.\nWant me to fix the LOW findings?")}, false, true},
		{"your call follow-up", []string{user, text("Your call on the follow-up.")}, false, true},
		{"ask question", []string{user, ask}, false, true},
		{"already blocked", []string{user, text("Want me to fix it?")}, true, false},
		{"plain report", []string{user, text("DONE. Fixed 3 findings; gates green.")}, false, false},
		{"requirement question", []string{user, text("Two readings: A or B. Which do you want?")}, false, false},
		{"old turn", []string{text("Want me to fix it?"), user, text("DONE.")}, false, false},
		{"no transcript", nil, false, false},
		{"answered question", []string{user, ask, result, text("Fixed them; gates green.")}, false, false},
		{"second text block", []string{user, twoBlocks}, false, true},
		{"meta record after", []string{user, text("Want me to fix it?"), meta}, false, true},
		{"huge final record", []string{user, huge}, false, true},
		{"line break", []string{user, text("The review found a race.\nWant me to address it?")}, false, true},
		{"would you like", []string{user, text("Would you like me to fix the findings?")}, false, true},
		{"shall i", []string{user, text("Shall I fix the LOW findings?")}, false, true},
		{"quoted question", []string{user, text("Fixed all findings. I will never ask \u2018want me to fix it?\u2019 again.")}, false, false},
		{"negated call", []string{user, text("Fixed the findings. Your call is no longer needed.")}, false, false},
		{"blockquote", []string{user, text("> Want me to fix the LOW findings?\nDONE.")}, false, false},
		{"filename dot", []string{user, text("There is a bug in api.go. Want me to proceed?")}, false, true},
		{"unrelated result", []string{user, askID, otherUse, otherResult}, false, true},
		{"option description", []string{user, transcriptAsk("Which editor should I open?", "Quick fixes")}, false, false},
	} {
		if r := stopResult(t, c.lines, c.stop); (r.Decision == core.Block) != c.want || (c.want && !strings.Contains(r.Reason, "Decide and act")) {
			t.Errorf("%s: got %+v, want block=%v", c.name, r, c.want)
		}
	}
}

func TestReviewDecideSkipsDispatchedAgents(t *testing.T) {
	t.Setenv(proc.DispatchIDEnv, "d1")
	if r := stopResult(t, []string{transcriptText("Want me to fix it?")}, false); r.Decision == core.Block {
		t.Error("a dispatched agent was blocked")
	}
}

// lastText is the final text block of the last transcript line, as Stop's last_assistant_message carries it.
func lastText(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	_, blocks, _ := turnEntry([]byte(lines[len(lines)-1]))
	for _, b := range slices.Backward(blocks) {
		if b.Type == "text" {
			return b.Text[max(0, len(b.Text)-60):]
		}
	}
	return ""
}
