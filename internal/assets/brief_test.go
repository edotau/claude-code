package assets

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
)

func TestBriefTextMatchesOldLiterals(t *testing.T) {
	const clean = "VERDICT: no actionable findings"
	checkFrames(t, []frameCase{
		{"review-loop", ReviewLoop(), "Review loop (standing rule): do not get a review of your own change; the main session runs one shared other-family\n" +
			"review over the collector at the end and sends back what it finds. Fix every finding you are sent yourself, any\n" +
			"severity. Never hand back a finding unfixed\n" +
			"or ask whether to fix it: fix it. Hand back only what you could not fix and why; the main session decides the rest.\n" +
			"A test your change breaks: fix it now if small (repair the test only when the test is wrong; never delete or skip\n" +
			"it); if large, hand back naming it for a test-repair leaf.\n" +
			"End every hand-back with exactly one line: \"UNFIXED: none\" or \"UNFIXED: <item>; <item>\"."},
		{"review-neutral", ReviewNeutral(), "Review the attached diff for correctness, regressions, missing tests, and scope creep.\n" +
			"Findings as one per line: [SEVERITY] file:line — issue — fix, ordered CRITICAL to LOW.\n" +
			"The first line of your answer is the verdict alone: SHIP, FIX REQUIRED or BLOCK."},
		{"review-adversarial", ReviewAdversarial(clean), "You are an adversarial reviewer: find what the author missed, not what is correct. Review the attached diff in\n" +
			"priority order — CRITICAL security, data loss, races, leaks; HIGH bugs, broken error handling, API misuse;\n" +
			"MEDIUM missing tests for new behavior, brittle assumptions, unhandled edge cases; LOW maintainability.\n" +
			"Skepticism ON: name the hidden assumption each change depends on. No praise, no summary — findings only, one\n" +
			"per line: <severity> <file:line> <issue> <fix>. If the diff is clean, say exactly: " + clean + "."},
		{"takeover", TakeoverBrief("codex", "deadline", "the task", 4, "tail"), fmt.Sprintf("A carried %s run was cut off (%s) before it finished.\n"+
			"Continue the task below from where it stopped and finish it; say what you changed.\n"+
			"Read every file named below that you need, and batch all of those independent reads into one response.\n"+
			"Keep it small: read only what the task or the partial output names, never survey the repository to start over.\n\n"+
			"Task:\n%s\n\nPartial output (last %d bytes):\n%s\n", "codex", "deadline", "the task", 4, "tail")},
		{"planner", LoopPlanner("G", 2, 4, 12, 40, 1500, ""), "You are the planner of an orchestrator loop. Goal:\n\nG\n\n" +
			"This is round 2 of at most 4. Decide what must still be done and answer with the JSON the schema asks for.\n\n" +
			"Rules for the parts you plan:\n" +
			"- Each part is independent of the others in its round and small enough to finish in a few minutes.\n" +
			"- A part's prompt is self-contained: it names the files and directories it needs by absolute path.\n" +
			"- At most 12 parts per round; ids are unique, up to 40 characters of letters, digits, dot, underscore and dash.\n" +
			"- You plan parts and do not edit files yourself; each part does its own work. List \"owns\" only for files or\n" +
			"  directories a part would be the sole writer of; the parts of one round must not share an owned path.\n" +
			"- Set \"agent\", \"model\" or \"timeout\" on a part only when you have a reason; leave them out otherwise.\n" +
			"- Do not repeat a part whose answer is already in the earlier rounds below; build on it, or replan it differently.\n" +
			"- When the goal is met, answer done=true with parts=[] and a summary of the result for the owner.\n" +
			"- Always fill \"summary\": what is known so far.\n\n" +
			"Earlier rounds (id, status, answer cut to 1500 characters):\n(none yet)\n"},
	})
}

// The old brief.tmpl was too long to restate: its text/template render is pinned by hash, per rtk mode.
func TestBriefBodyMatchesOldRender(t *testing.T) {
	for rtk, want := range map[bool]string{
		true:  "8d9d98bcbf8ea600c9d89fb069fcb451aa0fec43078a2b4f807d9f386bf43b3a",
		false: "3805464dc634801e991a261e3181f26e078aeac25d7a380e080c61cd34bd65e1",
	} {
		got := BriefBody(BriefFrame{Agent: "a", Verify: "v", Report: "r", Task: "T", OwnedLine: `["x.go"]`, ReportsDir: "/R", Rtk: rtk})
		if sum := fmt.Sprintf("%x", sha256.Sum256([]byte(got))); sum != want {
			t.Errorf("rtk=%v brief hash %s, want %s", rtk, sum, want)
		}
	}
}

// Only a pi brief tells the model to finish with the typed handback call; the pi text is pinned by hash.
func TestBriefBodyPiHandback(t *testing.T) {
	in := BriefFrame{Agent: "pi", Verify: "v", Report: "r", Task: "T", OwnedLine: `["x.go"]`, ReportsDir: "/R", Rtk: true}
	got := BriefBody(in)
	if !strings.Contains(got, "calling the `handback` tool once, last") {
		t.Fatal("pi brief lacks the handback instruction")
	}
	in.Agent = "codex"
	if strings.Contains(BriefBody(in), "handback") {
		t.Fatal("another agent's brief names handback")
	}
}

func TestBriefBodyPiMatchesHash(t *testing.T) {
	for rtk, want := range map[bool]string{
		true:  "6aa0622765d9f31618167cb8c78c3da04e65122e7e749cd9f89897c3fd69933b",
		false: "f4e88d0a76de912328a3541da2bf33608e1588d75039644e9e1f25dbc3f17ecf",
	} {
		got := BriefBody(BriefFrame{Agent: "pi", Verify: "v", Report: "r", Task: "T", OwnedLine: `["x.go"]`, ReportsDir: "/R", Rtk: rtk})
		if sum := fmt.Sprintf("%x", sha256.Sum256([]byte(got))); sum != want {
			t.Errorf("rtk=%v pi brief hash %s, want %s", rtk, sum, want)
		}
	}
}
