package assets

import "testing"

func TestSafetyTextMatchesOldLiterals(t *testing.T) {
	const route = " The owner can run it with `!` in the prompt."
	checkFrames(t, []frameCase{
		{"message", SafetyMessage("BLOCKED", "what", "pat", "cmd", "hint"), "safety: BLOCKED — what\n  pattern: pat\n  command: cmd\nhint\n"},
		{"message-no-pattern", SafetyMessage("ASK", "what", "", "cmd", "hint"), "safety: ASK — what\n  command: cmd\nhint\n"},
		{"no-ask", SafetyNoAsk("codex"), " (no ask decision on codex)"},
		{"arg-missing", SafetyArgMissing("WebFetch", "url"), "WebFetch call has no url string; blocked"},
		{"route", SafetyOwnerRoute(), route},
		{"attribution", SafetyAttributionWhat() + "|" + SafetyAttributionHint(),
			"AI-attribution footer in a commit, merge, tag or PR message (CLAUDE.md hard rule 3)|" +
				"Remove the Co-Authored-By / Generated-with lines and run it again; that needs no approval."},
		{"deny", SafetyDenyWhat() + "|" + SafetyDenyHint(), "matches a settings.json deny rule|" +
			"Change permissions.deny in ~/.claude/settings.json if this is wrong."},
		{"sudo", SafetySudoWhat() + "|" + SafetySudoHint(), "sudo command|sudo is not run from an agent session; " +
			"install to a user prefix (~/.local, pipx, a venv) instead."},
		{"protected", SafetyProtectedWhat() + "|" + SafetyProtectedHint(),
			"writes a file Edit/Write may never touch|Edit it by hand, or run it yourself in a terminal."},
		{"interpreter", SafetyInterpreterWhat() + "|" + SafetyInterpreterHint(),
			"an interpreter script that may write a file Edit/Write may never touch|" +
				"Approve only if you asked for it; otherwise edit it by hand or run it yourself in a terminal."},
		{"unresolved", SafetyPathUnresolved(), "path cannot be resolved to an absolute one; blocked"},
		{"rtk-note", RtkNote("rtk"), "rtk: output summarized; exact bytes: rtk bypass <cmd>"},
		{"rtk-shadow-none", RtkShadowNone("/b/rtk"), "rtk: no rtk on PATH, so the pinned /b/rtk runs by path — run make install"},
		{"rtk-shadow-found", RtkShadowFound("/x/rtk"), "rtk: rtk on PATH is /x/rtk, not the pinned binary — run make install"},
		{"claude-print", ClaudePrintHint(), "headless claude print runs skip dispatch records, routing and recall; " +
			"use `claude-code agents ask --overlay '{\"agent\":\"claude\"}' \"<task>\"` instead"},
		{"copilot-deny", CopilotDenyFeedback(), "Tool use is disabled for this headless run. Answer from the prompt alone."},
	})
}
