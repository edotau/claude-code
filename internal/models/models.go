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

const marker1M = "[1m]"

const (
	ContextWindow1M  = 1_000_000
	ContextWindowStd = 200_000
	// DatabricksPrefix is the serving-endpoint prefix; build full ids with Wire("databricks-serving", id).
	DatabricksPrefix = "databricks-"
	thinkingStd      = "31999"
	thinkingHigh     = "64000"
)

// Tolerates vendor prefixes like "anthropic/", "us.anthropic." (Bedrock) or "databricks-" (serving) before "claude-".
var claudeRE = regexp.MustCompile(`(?:^|[/.:-])claude-(opus|sonnet|haiku|fable|mythos)-(\d+)(?:[-.](\d+))?(?:-(\d{8}))?`)

// ParseClaudeID extracts (family, major, minor) from dashed or dotted ids; a >2-digit trailing group is a date stamp.
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

// Strip1M removes the client-only [1m] marker; no upstream accepts it on the wire.
func Strip1M(id string) string { return strings.TrimSuffix(id, marker1M) }

// Has1M reports a trailing client-only [1m] marker.
func Has1M(id string) bool { return strings.HasSuffix(id, marker1M) }

var canonicalPrefixes = [...]string{"databricks-", "system.ai.", "system.ai-"}

func isFamily(s string) bool { return s == Opus || s == Sonnet || s == Haiku || s == Fable }

// hasFamilyDash reports a bare "<family>-..." id (no claude- prefix).
func hasFamilyDash(s string) bool {
	i := strings.IndexByte(s, '-')
	return i > 0 && isFamily(s[:i])
}

// Canonical reduces a typed model id to the plain Claude id and reports a trailing [1m]; non-Claude ids pass through.
func Canonical(id string) (base string, oneM bool) {
	id = strings.TrimSpace(id)
	oneM = Has1M(id)
	id = Strip1M(id)
	if strings.Contains(id, "/") {
		return id, oneM
	}
	rest, stripped := id, false
	for _, p := range canonicalPrefixes {
		if strings.HasPrefix(id, p) {
			rest, stripped = id[len(p):], true
			break
		}
	}
	cand := rest
	if hasFamilyDash(rest) {
		cand = "claude-" + rest
	}
	if stripped || cand != rest {
		if _, _, _, ok := ParseClaudeID(cand); ok {
			return cand, oneM
		}
	}
	return id, oneM
}

// ContextWindow is 1M for a catalog Claude model or a [1m]-marked id, else 200K.
func ContextWindow(id string) int {
	if Has1M(id) {
		return ContextWindow1M
	}
	if canonical, _, ok := Resolve(id); ok && FamilyOf(canonical) != "" && inCatalog(canonical) {
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

// Family is a model's vendor family, decided once here for every "is Claude / GPT / Gemini" check.
type Family string

const (
	FamilyClaude Family = "claude"
	FamilyGPT    Family = "gpt"
	FamilyGemini Family = "gemini"
	FamilyOther  Family = "other"
)

// legacyClaudeRE matches Claude ids the catalog and ParseClaudeID do not list (claude-3-7-sonnet, us.anthropic.claude-...).
var legacyClaudeRE = regexp.MustCompile(`(?:^|[/.:-])claude-`)

// openAIReasoningRE: OpenAI reasoning models (o-series, gpt-5 and newer) reject max_tokens, temperature and top_p.
var openAIReasoningRE = regexp.MustCompile(`^(?:o\d|gpt-(?:[5-9]|[1-9]\d))(?:\D|$)`)

// normID is the catalog id of s when it resolves, else s lowered, [1m]-stripped and without vendor or wire prefix.
func normID(s string) string {
	if canonical, _, ok := Resolve(s); ok {
		return canonical
	}
	s = Strip1M(strings.ToLower(strings.TrimSpace(s)))
	if _, after, ok := strings.CutLast(s, "/"); ok {
		s = after
	}
	for _, p := range wirePrefixes {
		if rest, ok := strings.CutPrefix(s, p); ok {
			return rest
		}
	}
	return s
}

// FamilyKind classifies id by vendor family through the catalog (any spelling), falling back to the name for unlisted ids.
func FamilyKind(id string) Family {
	n := normID(id)
	switch {
	case legacyClaudeRE.MatchString(n):
		return FamilyClaude
	case strings.HasPrefix(n, "gpt-") || openAIReasoningRE.MatchString(n):
		return FamilyGPT
	case strings.HasPrefix(n, "gemini-"):
		return FamilyGemini
	}
	return FamilyOther
}

// IsOpenAIReasoning reports an OpenAI reasoning model (o-series, gpt-5 and newer) under any spelling or vendor prefix.
func IsOpenAIReasoning(id string) bool { return openAIReasoningRE.MatchString(normID(id)) }

// ResponsesOnly reports a catalog model served on the Responses API only; false for an unlisted id.
func ResponsesOnly(id string) bool {
	canonical, _, ok := Resolve(id)
	return ok && load().byID[canonical].ResponsesOnly
}

// ChatOnly reports a catalog model the Responses API does not serve (open-weight gpt-oss); false for an unlisted id.
func ChatOnly(id string) bool {
	canonical, _, ok := Resolve(id)
	return ok && load().byID[canonical].ChatOnly
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

func inCatalog(canonical string) bool { _, ok := Lookup(canonical); return ok }

// Effort tiers; the strings equal the hooks package's TierLow/Medium/High.
const (
	EffortLow    = "low"
	EffortMedium = "medium"
	EffortHigh   = "high"
)

// TierModel is the Agent-tool alias for an effort tier ("" for an unknown tier); an alias, so `models --pin` applies.
func TierModel(tier string) string {
	switch tier {
	case EffortLow:
		return Haiku
	case EffortMedium, EffortHigh:
		return Sonnet
	}
	return ""
}
