// The prompt complexity classifier behind EffortTier: read by the agent-effort hook (fills an unset
// dispatch model). Advisory and O(len(prompt)) — never a gate.
package hooks

import (
	"regexp"
	"strings"
	"sync"
)

// complexity is the classifier verdict; neutral emits no extra context.
type complexity int

const (
	complexityNeutral complexity = iota
	complexityLow
	complexityHigh
)

var (
	// filePathRe matches file-ish tokens (foo/bar.go, ./x.py, io.go) — a path signals real code work.
	filePathRe = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`(?:^|\s)\.{0,2}/?(?:[\w.-]+/)*[\w-]+\.(?:go|py|md|sh|bash|js|ts|tsx|jsx|yaml|yml|json|toml|sql|rs|java|c|h|cpp|rb|css|html|tmpl)\b`)
	})
	questionRe = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`(?i)^(what|where|why|how|when|which|who|explain|show|list|is|are|does|do|can|should)\b`)
	})
	// highRe: keywords that mark multi-file / design-level work regardless of prompt length.
	highRe = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`(?i)\b(refactor|redesign|design|migrate|migration|implement|architecture|architect|audit|overhaul|rewrite)\b`)
	})
	mechanicalRe = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`(?i)\b(typo|rename|one[- ]liner)\b`)
	})
	// scopeRe: a mechanical verb spanning many sites is multi-file work, not a one-liner — stays neutral.
	scopeRe = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`(?i)\b(across|every|all|repo-wide|codebase|call ?sites|callers|everywhere|throughout)\b`)
	})
)

// classifyPrompt is a cheap heuristic complexity read of the raw prompt. It must stay O(len(prompt))
// and advisory-only: any uncertainty returns neutral so no routing hint is emitted.
func classifyPrompt(prompt string) complexity {
	p := strings.TrimSpace(prompt)
	if p == "" {
		return complexityNeutral
	}
	paths := filePathRe().FindAllString(p, 3)
	if highRe().MatchString(p) || len(paths) >= 2 || isLongSpec(p) {
		return complexityHigh
	}
	if len(p) < 120 && len(paths) == 0 && !strings.Contains(p, "```") {
		if questionRe().MatchString(p) || strings.HasSuffix(p, "?") || (mechanicalRe().MatchString(p) && !scopeRe().MatchString(p)) {
			return complexityLow
		}
	}
	return complexityNeutral
}

// Effort tiers EffortTier returns; they match the Claude Code effort vocabulary (low|medium|high).
const (
	TierLow    = "low"
	TierMedium = "medium"
	TierHigh   = "high"
)

// EffortTier is the exported seam onto classifyPrompt (single public entry point). Neutral maps to medium.
func EffortTier(prompt string) string {
	switch classifyPrompt(prompt) {
	case complexityLow:
		return TierLow
	case complexityHigh:
		return TierHigh
	default:
		return TierMedium
	}
}

// isLongSpec: a long prompt with several sentences reads as a spec, not a question.
func isLongSpec(p string) bool {
	if len(p) < 400 {
		return false
	}
	sentences := strings.Count(p, ". ") + strings.Count(p, ".\n") + strings.Count(p, "\n- ") + strings.Count(p, "\n\n")
	return sentences >= 3
}
