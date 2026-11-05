package shell

import (
	"regexp"
	"slices"
	"sync"
)

// footerPatterns match an agent-attribution footer anywhere in the raw command text (CLAUDE.md hard rule 3).
var footerPatterns = sync.OnceValue(func() []*regexp.Regexp {
	return []*regexp.Regexp{
		regexp.MustCompile(`(?i)co-authored-by:\s*(claude|codex|copilot|gemini|opencode)\b`),
		regexp.MustCompile(`(?i)generated (with|by) \[?(claude code|codex|github copilot|copilot|gemini|opencode)\b`),
	}
})

// AttributionFooterCommit reports a `git commit` whose WHOLE command text (heredoc bodies too) carries a footer.
func AttributionFooterCommit(cmd string) bool {
	footer := slices.ContainsFunc(footerPatterns(), func(re *regexp.Regexp) bool { return re.MatchString(cmd) })
	return footer && GitCommitInvocation(cmd)
}
