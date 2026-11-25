package quality

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/proc"
)

func TestReviewHandback(t *testing.T) {
	const noLine = "The leaf report has no UNFIXED line; read it and fix anything left before reporting."
	const fix = "Fix these now; do not relay them to the owner as questions:\n"
	for _, c := range []struct{ name, report, want string }{
		{"none", "DONE.\nUNFIXED: none", ""},
		{"none cased", "DONE.\n**UNFIXED: None.**", ""},
		{"list", "DONE.\nUNFIXED: parser leak", fix + "- parser leak"},
		{"items", "UNFIXED: parser leak; race in api.go", fix + "- parser leak\n- race in api.go"},
		{"last line wins", "UNFIXED: old\nmore\nUNFIXED: none", ""},
		{"empty footer", "UNFIXED: race\nUNFIXED:", noLine},
		{"blank items", "UNFIXED: ; ;", noLine},
		{"overflow", "UNFIXED: " + strings.Repeat("x;", 14), fix + strings.Repeat("- x\n", 12) + "- and 2 more: read the full UNFIXED line"},
		{"no line", "DONE. Left as-is: naming.", noLine},
	} {
		in := core.ParseInput(strings.NewReader(`{"tool_name":"Agent","tool_response":{"content":[{"type":"text","text":` +
			jsonStr(c.report) + `}],"prompt":"UNFIXED: echoed"}}`))
		if r := ReviewHandback(in); r.Context != c.want || r.Decision != core.Allow {
			t.Errorf("%s: context %q decision %v, want %q and no block", c.name, r.Context, r.Decision, c.want)
		}
	}
}

func TestReviewHandbackVerdictFromFindings(t *testing.T) {
	const mm = "The report states %s but its CRITICAL/HIGH findings compute %s. Treat the computed verdict as the verdict."
	for _, c := range []struct{ name, report, want string }{
		{"ship with high", "SHIP\n[HIGH] a.go:3 — leak — close\nUNFIXED: none", fmt.Sprintf(mm, "SHIP", "FIX REQUIRED")},
		{"fix with critical", "FIX REQUIRED\n[CRITICAL] a.go:3 — x — y\nUNFIXED: none", fmt.Sprintf(mm, "FIX REQUIRED", "BLOCK")},
		{"consistent", "BLOCK\n[CRITICAL] a.go:3 — x — y\nUNFIXED: none", ""},
		{"ship with medium", "SHIP\n[MEDIUM] a.go:3 — x — y\nUNFIXED: none", ""},
		{"numbered bold", "SHIP\n1. **[HIGH]** a.go:3 — x — y\nUNFIXED: none", fmt.Sprintf(mm, "SHIP", "FIX REQUIRED")},
		{"quoted finding", "SHIP\n> [HIGH] a.go:3 — old\n```\n[CRITICAL] b.go:1 x\n```\nUNFIXED: none", ""},
		{"note after unfixed", "SHIP\n[HIGH] a.go:3 — x — y\nUNFIXED: race", "Fix these now; do not relay them to the owner as questions:\n- race\n" + fmt.Sprintf(mm, "SHIP", "FIX REQUIRED")},
		{"no verdict line", "[HIGH] a.go:3 — x — y\nUNFIXED: none", ""},
	} {
		in := core.ParseInput(strings.NewReader(`{"tool_response":{"content":[{"type":"text","text":` + jsonStr(c.report) + `}]}}`))
		if r := ReviewHandback(in); r.Context != c.want || r.Decision != core.Allow {
			t.Errorf("%s: context %q, want %q", c.name, r.Context, c.want)
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
	askLabel := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"c3","name":"AskUserQuestion","input":{"questions":[` +
		`{"question":"Next step?","options":[{"label":"Run review"}]}]}}]}}`
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
		{"ask question", []string{user, ask}, false, true},
		{"ask review label", []string{user, askLabel}, false, true},
		{"already blocked", []string{user, text("Want me to fix it?")}, true, false},
		{"plain report", []string{user, text("DONE. Fixed 3 findings; gates green.")}, false, false},
		{"requirement question", []string{user, text("Two readings: A or B. Which do you want?")}, false, false},
		{"old turn", []string{text("Want me to fix it?"), user, text("DONE.")}, false, false},
		{"no transcript", nil, false, false},
		{"answered question", []string{user, ask, result, text("Fixed them; gates green.")}, false, false},
		{"second text block", []string{user, twoBlocks}, false, true},
		{"meta record after", []string{user, text("Want me to fix it?"), meta}, false, true},
		{"huge final record", []string{user, huge}, false, true},
		{"would you like", []string{user, text("Would you like me to fix the findings?")}, false, true},
		{"shall i", []string{user, text("Shall I fix the LOW findings?")}, false, true},
		{"quoted question", []string{user, text("I will never ask \u2018want me to fix it?\u2019 again.")}, false, false},
		{"blockquote", []string{user, text("> Want me to fix the LOW findings?\nDONE.")}, false, false},
		{"fixture question", []string{user, transcriptAsk("Which fixture should the example use?", "")}, false, false},
		{"tilde fence", []string{user, text("~~~\nWant me to fix it?\n~~~\nDONE.")}, false, false},
		{"indented code", []string{user, text("Example:\n    Want me to fix it?\nDONE.")}, false, false},
		{"question after indented code", []string{user, text("Example:\n    go test ./x\nWant me to fix the LOW findings?")}, true, false},
		{"multiline quote", []string{user, text("He said \"ok.\nWant me to fix it?\" done.")}, false, false},
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
	_, blocks, _ := TurnEntry([]byte(lines[len(lines)-1]))
	for _, b := range slices.Backward(blocks) {
		if b.Type == "text" {
			return b.Text[max(0, len(b.Text)-60):]
		}
	}
	return ""
}
