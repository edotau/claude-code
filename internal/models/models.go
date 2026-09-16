// Package models parses Claude model ids and derives the per-model Claude Code knobs (1M context, thinking).
package models

import (
	"regexp"
	"strconv"
	"strings"
)

// Slots are Claude Code's model tiers; each maps to ANTHROPIC_DEFAULT_<SLOT>_MODEL.
const (
	Opus   = "opus"
	Sonnet = "sonnet"
	Haiku  = "haiku"
	Fable  = "fable"
)

// Slots lists every tier in precedence order (the opus slot is the session default).
var Slots = []string{Opus, Sonnet, Haiku, Fable}

const (
	ContextWindow1M  = 1_000_000
	ContextWindowStd = 200_000
	thinkingStd      = "31999"
	thinkingHigh     = "64000"
)

// Tolerates vendor prefixes like "anthropic/" (OpenRouter) or "us.anthropic." (Bedrock) before "claude-".
var claudeRE = regexp.MustCompile(`(?:^|[/.:])claude-(opus|sonnet|haiku|fable|mythos)-(\d+)(?:-(\d+))?`)

// ParseClaudeID extracts (family, major, minor); a >2-digit trailing group is a date stamp, not a minor.
func ParseClaudeID(id string) (family string, major, minor int, ok bool) {
	m := claudeRE.FindStringSubmatch(Strip1M(id))
	if m == nil {
		return "", 0, 0, false
	}
	major, _ = strconv.Atoi(m[2])
	if m[3] != "" && len(m[3]) <= 2 {
		minor, _ = strconv.Atoi(m[3])
	}
	return m[1], major, minor, true
}

func atLeast(major, minor, wantMajor, wantMinor int) bool {
	return major > wantMajor || (major == wantMajor && minor >= wantMinor)
}

// MaybeAdd1M appends Claude Code's [1m] marker for opus/sonnet >= 4.6; the client swaps it for the 1M beta header.
func MaybeAdd1M(id string) string {
	if strings.HasSuffix(id, "[1m]") {
		return id
	}
	if f, maj, min, ok := ParseClaudeID(id); ok && (f == Opus || f == Sonnet) && atLeast(maj, min, 4, 6) {
		return id + "[1m]"
	}
	return id
}

// Strip1M removes the client-only [1m] marker; no upstream accepts it on the wire.
func Strip1M(id string) string { return strings.TrimSuffix(id, "[1m]") }

// ContextWindow is 1M for [1m]-marked ids, else 200K.
func ContextWindow(id string) int {
	if strings.HasSuffix(id, "[1m]") {
		return ContextWindow1M
	}
	return ContextWindowStd
}

// ThinkingKnobs returns (MAX_THINKING_TOKENS, CLAUDE_CODE_DISABLE_ADAPTIVE_THINKING); adaptive-only models reject a budget.
func ThinkingKnobs(id string) (maxTokens, disableAdaptive string) {
	f, maj, min, ok := ParseClaudeID(id)
	if !ok {
		return "", ""
	}
	if f != Haiku && atLeast(maj, min, 4, 8) {
		return "", ""
	}
	if f == Opus && maj == 4 && min == 7 && ContextWindow(id) == ContextWindow1M {
		return thinkingHigh, ""
	}
	if f == Opus && maj == 4 && min == 6 {
		return thinkingStd, "1"
	}
	return thinkingStd, ""
}

// FamilyOf returns the Claude family of id, or "" for non-Claude ids.
func FamilyOf(id string) string {
	f, _, _, _ := ParseClaudeID(id)
	if f == "mythos" {
		return Opus
	}
	return f
}
