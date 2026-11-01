package agent

import "strings"

// CopilotTokenEnv is copilot's highest-precedence token var; the harness sets it at launch from `gh auth token`.
const CopilotTokenEnv = "COPILOT_GITHUB_TOKEN"

// copilotTokenPrefixes are GitHub's own token prefixes (gho_/ghp_/ghu_) plus the newer fine-grained github_pat_.
var copilotTokenPrefixes = []string{"gho_", "ghp_", "ghu_", "github_pat_"}

// ValidCopilotToken reports whether tok has the shape of a real GitHub token, rejecting an obvious
// typo or truncated paste before it is sealed under CopilotTokenEnv.
func ValidCopilotToken(tok string) bool {
	if len(tok) < 20 {
		return false
	}
	for _, p := range copilotTokenPrefixes {
		if strings.HasPrefix(tok, p) {
			return true
		}
	}
	return false
}
