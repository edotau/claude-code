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
	// openSearchRe: the task is a hunt with no known fix target yet — needs the stronger model.
	openSearchRe = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`(?i)\b(investigate|find out|track down|root[- ]cause|debug|diagnose)\b`)
	})
	// liveSystemRe: anything touching a live/production system raises the bar regardless of length.
	liveSystemRe = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`(?i)\b(production|prod|live|cluster|deploy(?:ment)?|database)\b`)
	})
)

// classifyPromptSignal is a cheap O(len(prompt)) complexity read plus the rule that fired; uncertainty is neutral.
func classifyPromptSignal(prompt string) (complexity, string) {
	p := strings.TrimSpace(prompt)
	if p == "" {
		return complexityNeutral, "neutral"
	}
	paths := filePathRe().FindAllString(p, 3)
	if m := highRe().FindString(p); m != "" {
		return complexityHigh, "keyword:" + strings.ToLower(m)
	}
	if openSearchRe().MatchString(p) {
		return complexityHigh, "open-search"
	}
	if liveSystemRe().MatchString(p) {
		return complexityHigh, "live-system"
	}
	if len(paths) >= 2 {
		return complexityHigh, "paths>=2"
	}
	if isLongSpec(p) {
		return complexityHigh, "long-spec"
	}
	if len(p) < 120 && len(paths) == 0 && !strings.Contains(p, "```") {
		if questionRe().MatchString(p) || strings.HasSuffix(p, "?") {
			return complexityLow, "question"
		}
		if mechanicalRe().MatchString(p) && !scopeRe().MatchString(p) {
			return complexityLow, "mechanical"
		}
	}
	// A single touched file plus a mechanical verb and no scope word is still a one-liner, just
	// one that names its target — allow a longer prompt than the path-less case above.
	if len(paths) == 1 && len(p) < 200 && mechanicalRe().MatchString(p) && !scopeRe().MatchString(p) {
		return complexityLow, "mechanical"
	}
	return complexityNeutral, "neutral"
}

// Effort tiers EffortTier returns; they match the Claude Code effort vocabulary (low|medium|high).
const (
	TierLow    = "low"
	TierMedium = "medium"
	TierHigh   = "high"
)

// EffortTier is the exported seam onto classifyPromptSignal. Neutral maps to medium.
func EffortTier(prompt string) string {
	tier, _ := EffortTierSignal(prompt)
	return tier
}

// EffortTierSignal is EffortTier plus the short rule name that fired (e.g. "keyword:refactor",
// "paths>=2", "open-search"), for a hook that wants to say WHY it picked a tier.
func EffortTierSignal(prompt string) (tier, signal string) {
	c, signal := classifyPromptSignal(prompt)
	switch c {
	case complexityLow:
		return TierLow, signal
	case complexityHigh:
		return TierHigh, signal
	default:
		return TierMedium, signal
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
