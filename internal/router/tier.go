// The prompt complexity classifier behind EffortTierSignal: read by the agent-effort hook (fills an unset
// dispatch model). Advisory and O(len(prompt)) — never a gate.
package router

import (
	"regexp"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/models"
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
	if rule := highSignal(p, len(paths)); rule != "" {
		return complexityHigh, rule
	}
	if rule := lowSignal(p, len(paths)); rule != "" {
		return complexityLow, rule
	}
	return complexityNeutral, "neutral"
}

// shortPromptRule names why a short, path-less prompt is low complexity: a question or a scoped mechanical verb.
func shortPromptRule(p string) string {
	if questionRe().MatchString(p) || strings.HasSuffix(p, "?") {
		return "question"
	}
	if mechanicalRe().MatchString(p) && !scopeRe().MatchString(p) {
		return "mechanical"
	}
	return ""
}

// highSignal names the first rule that reads p as high complexity, or "".
func highSignal(p string, paths int) string {
	switch {
	case highRe().MatchString(p):
		return "keyword:" + strings.ToLower(highRe().FindString(p))
	case openSearchRe().MatchString(p):
		return "open-search"
	case liveSystemRe().MatchString(p):
		return "live-system"
	case paths >= 2:
		return "paths>=2"
	case isLongSpec(p):
		return "long-spec"
	}
	return ""
}

// lowSignal names the rule that reads p as low complexity (question or mechanical one-liner), or "".
func lowSignal(p string, paths int) string {
	if len(p) < 120 && paths == 0 && !strings.Contains(p, "```") {
		if rule := shortPromptRule(p); rule != "" {
			return rule
		}
	}
	// A single touched file plus a mechanical verb and no scope word is still a one-liner, just
	// one that names its target — allow a longer prompt than the path-less case above.
	if paths == 1 && len(p) < 200 && mechanicalRe().MatchString(p) && !scopeRe().MatchString(p) {
		return "mechanical"
	}
	return ""
}

// EffortTierSignal is the exported seam onto classifyPromptSignal plus the short rule name that fired (e.g. "keyword:refactor",
// "paths>=2", "open-search"), for a hook that wants to say WHY it picked a tier.
func EffortTierSignal(prompt string) (tier, signal string) {
	c, signal := classifyPromptSignal(prompt)
	switch c {
	case complexityLow:
		return models.EffortLow, signal
	case complexityHigh:
		return models.EffortHigh, signal
	default:
		return models.EffortMedium, signal
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

// RuleClassifier is route.Classifier over the prompt rules: low → haiku, medium/high → sonnet.
type RuleClassifier struct{}

// Alias maps task through EffortTierSignal and models.AliasModel.
func (RuleClassifier) Alias(task string) string {
	t, _ := EffortTierSignal(task)
	return models.AliasModel(t)
}
