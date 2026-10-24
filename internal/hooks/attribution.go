package hooks

import (
	"regexp"
	"slices"
)

// footerPatterns match an agent-attribution footer anywhere in the raw command text (CLAUDE.md hard rule 3).
var footerPatterns = lazyCompile(
	`(?i)co-authored-by:\s*(claude|codex|copilot|gemini|opencode)\b`,
	`(?i)generated (with|by) \[?(claude code|codex|github copilot|copilot|gemini|opencode)\b`,
)

// attributionFooterBlocked reports a `git commit` whose WHOLE command text (heredoc bodies too) carries a footer.
func attributionFooterBlocked(cmd string) bool {
	footer := slices.ContainsFunc(footerPatterns(), func(re *regexp.Regexp) bool { return re.MatchString(cmd) })
	return footer && gitCommitInvocation(cmd)
}

// gitCommitInvocation reports whether cmd, or a nested/substituted command within it, invokes `git commit`.
func gitCommitInvocation(cmd string) bool {
	masked, subs := extractSubstitutions(cmd)
	for _, sub := range subs {
		if gitCommitInvocation(sub) {
			return true
		}
	}
	for _, seg := range shellSegments(masked) {
		toks := shellTokens(seg)
		if len(toks) == 0 {
			continue
		}
		if nested, ok := nestedShellCommand(toks); ok && gitCommitInvocation(nested) {
			return true
		}
		for i, t := range toks {
			if t != "git" {
				continue
			}
			_, verb, _ := gitInvocation(toks[i+1:], "")
			if verb == "commit" {
				return true
			}
		}
	}
	return false
}
