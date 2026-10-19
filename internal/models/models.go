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

// NextSlot is the demotion ladder a quota-dead model falls down: opus → sonnet → haiku → "" (none). Fable sits
// outside it: a distinct family, not a cheaper tier of the same one.
func NextSlot(slot string) string {
	switch slot {
	case Opus:
		return Sonnet
	case Sonnet:
		return Haiku
	}
	return ""
}

const (
	ContextWindow1M  = 1_000_000
	ContextWindowStd = 200_000
	thinkingStd      = "31999"
	thinkingHigh     = "64000"
)

// Tolerates vendor prefixes like "anthropic/" (gateways) or "us.anthropic." (Bedrock) before "claude-".
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

// Use-case labels, keyed to Anthropic's own tier wording on the Claude model comparison page.
const (
	UseFrontier  = "Frontier reasoning" // demanding reasoning and long-horizon agentic work
	UseAgentic   = "Agentic coding"     // long-running agentic coding and knowledge work
	UseBalanced  = "Balanced"           // best combination of speed and intelligence
	UseHighVol   = "High volume"        // high-volume, latency-sensitive: classification, extraction, routing
	UseAuto      = "Auto"
	UseSpeech    = "Speech"
	UseEmbedding = "Embeddings"
	UseImage     = "Image"
	UseVideo     = "Video"
	UseGeneral   = "General"
)

// UseCase maps a model id (and optional display name) onto Anthropic's four tiers by family, or a
// non-chat kind. Pure and table-driven: no vendor exposes tier metadata, so this goes by id markers.
func UseCase(id, name string) string {
	s := strings.ToLower(id + " " + name)
	switch {
	case id == "auto":
		return UseAuto
	case containsAny(s, "whisper", "tts", "speech", "audio"):
		return UseSpeech
	case containsAny(s, "embed"):
		return UseEmbedding
	case containsAny(s, "dall-e", "dalle", "imagen", "image-gen", "-image"):
		return UseImage
	case containsAny(s, "video", "veo"):
		return UseVideo
	case FamilyOf(id) == Fable:
		return UseFrontier
	case FamilyOf(id) == Opus:
		return UseAgentic
	case FamilyOf(id) == Sonnet:
		return UseBalanced
	case FamilyOf(id) == Haiku:
		return UseHighVol
	case containsAny(s, "gemini", "gemma"):
		return geminiUseCase(s)
	case containsAny(s, "gpt", "o1", "o3", "o4"):
		return gptUseCase(s)
	default:
		return UseGeneral
	}
}

func geminiUseCase(s string) string {
	switch {
	case containsAny(s, "flash-lite", "gemma", "nano"):
		return UseHighVol
	case containsAny(s, "-pro"):
		return UseAgentic
	case containsAny(s, "-flash"):
		return UseBalanced
	default:
		return UseGeneral
	}
}

func gptUseCase(s string) string {
	switch {
	case containsAny(s, "mini", "nano"):
		return UseHighVol
	case containsAny(s, "o1", "o3", "o4") || containsAny(s, "-pro", "high"):
		return UseFrontier
	default:
		return UseAgentic
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
