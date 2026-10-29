package hooks

// OutputCodex is exactly one JSON document in Codex's Claude-compatible schema: no ask decision (deny), and an
// updatedInput on PreToolUse is an explicit allow (ruling 4).
func OutputCodex(event string, r Result) string {
	return claudeDoc(event, r, claudeStyle{askAsDeny: true, allowWithInput: true})
}
