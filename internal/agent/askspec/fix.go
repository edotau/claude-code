package askspec

import "slices"

// LeadingAgentWord is pos[0] when it is a roster agent name, --agent is untyped and no "--" precedes it.
func LeadingAgentWord(args, pos, roster []string, agentTyped bool) (string, bool) {
	if agentTyped || len(pos) == 0 || !slices.Contains(roster, pos[0]) {
		return "", false
	}
	if i := slices.Index(args, "--"); i >= 0 && len(pos) <= len(args)-i-1 {
		return "", false
	}
	return pos[0], true
}
