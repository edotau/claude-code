package assets

var (
	safetyTemplates = loadTemplates("safety/*.tmpl")
	rtkTemplates    = loadTemplates("rtk/*.tmpl")
)

func renderSafety(name string, input any) string { return renderFrom(safetyTemplates(), name, input) }

func renderRtk(name string, input any) string { return renderFrom(rtkTemplates(), name, input) }

// SafetyMessage is the stderr/reason text of a safety block or ask; pattern "" drops its line.
func SafetyMessage(word, what, pattern, command, hint string) string {
	return renderSafety("message.tmpl", tmplFields{
		"Word": word, "What": what, "Pattern": pattern, "Command": command, "Hint": hint,
	})
}

func SafetyNoAsk(agent string) string {
	return renderSafety("messages/no-ask", tmplFields{"Agent": agent})
}

func SafetyArgMissing(tool, key string) string {
	return renderSafety("messages/arg-missing", tmplFields{"Tool": tool, "Key": key})
}

func SafetyOwnerRoute() string { return string(read("safety/owner-route.txt")) }

func SafetyAttributionWhat() string { return string(read("safety/attribution-what.txt")) }

func SafetyAttributionHint() string { return string(read("safety/attribution-hint.txt")) }

func SafetyDenyWhat() string { return string(read("safety/deny-what.txt")) }

func SafetyDenyHint() string { return string(read("safety/deny-hint.txt")) }

func SafetySudoWhat() string { return string(read("safety/sudo-what.txt")) }

func SafetySudoHint() string { return string(read("safety/sudo-hint.txt")) }

func SafetyProtectedWhat() string { return string(read("safety/protected-what.txt")) }

func SafetyProtectedHint() string { return string(read("safety/protected-hint.txt")) }

func SafetyInstructionWhat() string { return string(read("safety/instruction-what.txt")) }

func SafetyInstructionHint() string { return string(read("safety/instruction-hint.txt")) }

func SafetyInterpreterWhat() string { return string(read("safety/interpreter-what.txt")) }

func SafetyInterpreterHint() string { return string(read("safety/interpreter-hint.txt")) }

func SafetyPathUnresolved() string { return string(read("safety/path-unresolved.txt")) }

func RtkNote(word string) string { return renderRtk("notes/note", tmplFields{"Word": word}) }

func RtkShadowNone(bin string) string { return renderRtk("notes/shadow-none", tmplFields{"Bin": bin}) }

func RtkShadowFound(found string) string {
	return renderRtk("notes/shadow-found", tmplFields{"Found": found})
}

func ClaudePrintHint() string { return string(read("rtk/claude-print-hint.txt")) }

func CopilotDenyFeedback() string { return string(read("copilot/deny-feedback.txt")) }
