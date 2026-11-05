package hooks

import (
	"regexp"
	"slices"

	"github.com/edotau/claude-code/internal/shell"
)

// footerPatterns match an agent-attribution footer anywhere in the raw command text (CLAUDE.md hard rule 3).
var footerPatterns = lazyCompile(
	`(?i)co-authored-by:\s*(claude|codex|copilot|gemini|opencode)\b`,
	`(?i)generated (with|by) \[?(claude code|codex|github copilot|copilot|gemini|opencode)\b`,
)

// attributionFooterBlocked reports a `git commit` whose WHOLE command text (heredoc bodies too) carries a footer.
func attributionFooterBlocked(cmd string) bool {
	footer := slices.ContainsFunc(footerPatterns(), func(re *regexp.Regexp) bool { return re.MatchString(cmd) })
	return footer && shell.GitCommitInvocation(cmd)
}
