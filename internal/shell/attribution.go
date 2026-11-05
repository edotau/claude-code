package shell

import (
	"regexp"
	"slices"
	"strings"
	"sync"
)

// footerPatterns match an agent-attribution footer in command text (CLAUDE.md hard rule 3).
var footerPatterns = sync.OnceValue(func() []*regexp.Regexp {
	return []*regexp.Regexp{
		regexp.MustCompile(`(?i)co-authored-by:\s*(claude|anthropic|codex|copilot|gemini|opencode)\b`),
		regexp.MustCompile(`(?i)generated (with|by)\s*\[?(claude( code)?|codex|github copilot|copilot|gemini|opencode)\b`),
	}
})

// footerMention is a looser test used when a message is built by a substitution whose output cannot be seen.
var footerMention = regexp.MustCompile(`(?i)co-authored-by|generated (with|by)`)

func hasFooter(text string) bool {
	return slices.ContainsFunc(footerPatterns(), func(re *regexp.Regexp) bool { return re.MatchString(text) })
}

// AttributionFooterCommit reports a `git commit` carrying a footer in its raw text (heredoc bodies too), in its
// unquoted arguments (`"Cl""aude"`, `$'\x20'`), or in text a substitution builds that mentions one.
func AttributionFooterCommit(cmd string) bool {
	if hasFooter(cmd) && GitCommitInvocation(cmd) {
		return true
	}
	_, subs := ExtractSubstitutions(cmd)
	dynamic := slices.ContainsFunc(subs, func(s string) bool { return !strings.Contains(s, "<<") })
	return EachGit(cmd, "", func(_, verb string, args []string) bool {
		if verb != "commit" {
			return false
		}
		joined := strings.Join(args, " ")
		return hasFooter(joined) || dynamic && strings.Contains(joined, subMask) && footerMention.MatchString(cmd)
	})
}
