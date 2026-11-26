// Package models parses Claude model ids and derives the per-model Claude Code knobs (1M context, thinking).
package models

import (
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/platform/textutil"
)

// AliasNames are Claude Code's model aliases; each maps to ANTHROPIC_DEFAULT_<SLOT>_MODEL.
const (
	Opus   = "opus"
	Sonnet = "sonnet"
	Haiku  = "haiku"
	Fable  = "fable"
)

// AliasNames lists every alias in precedence order (the opus alias is the session default).
var AliasNames = []string{Opus, Sonnet, Haiku, Fable}

// TierAliases maps a brief's `Tier:` word to the alias of that tier; AliasTier is its one reverse lookup.
var TierAliases = map[string]string{"fast": Haiku, "standard": Sonnet, "deep": Opus}

// AliasTier is the tier word whose alias is alias, "" for none (fable).
func AliasTier(alias string) string {
	for tier, a := range TierAliases {
		if a == alias {
			return tier
		}
	}
	return ""
}

// NextAlias demotes opus → sonnet → haiku → ""; Fable is a distinct family outside the ladder.
func NextAlias(alias string) string {
	switch alias {
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
	// ContextWindowUnknown is a non-Claude model's window when neither the catalog nor a listing gives one.
	ContextWindowUnknown = 128_000
	// DatabricksPrefix is the serving-endpoint prefix; build full ids with Wire("databricks-serving", id).
	DatabricksPrefix = "databricks-"
	thinkingStd      = "31999"
	thinkingHigh     = "64000"
)

// Tolerates vendor prefixes like "anthropic/", "us.anthropic." (Bedrock) or "databricks-" (serving) before "claude-".
var claudeRE = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?:^|[/.:-])claude-(opus|sonnet|haiku|fable|mythos)-(\d+)(?:[-.](\d+))?(?:-(\d{8}))?`)
})

// matchClaude is claudeRE's submatches of s; the literal precheck skips the regexp for every non-Claude id.
func matchClaude(s string) []string {
	if !strings.Contains(s, "claude-") {
		return nil
	}
	return claudeRE().FindStringSubmatch(s)
}

// ParseClaudeID extracts (family, major, minor) from dashed or dotted ids; a >2-digit trailing group is a date stamp.
func ParseClaudeID(id string) (family string, major, minor int, ok bool) {
	m := matchClaude(Strip1M(id))
	if m == nil {
		return "", 0, 0, false
	}
	major, _ = strconv.Atoi(m[2])
	if m[3] != "" && len(m[3]) <= 2 {
		minor, _ = strconv.Atoi(m[3])
	}
	return m[1], major, minor, true
}

// ClassifierModel is the first Sonnet or Haiku id the auto-mode classifier accepts (it ignores Sonnet 5.5).
func ClassifierModel(ids ...string) string {
	for _, id := range ids {
		family, major, minor, ok := ParseClaudeID(id)
		if id == "" || ok && (family != Sonnet && family != Haiku || family == Sonnet && major == 5 && minor == 5) {
			continue
		}
		return id
	}
	return ""
}

func atLeast(major, minor, wantMajor, wantMinor int) bool {
	return major > wantMajor || (major == wantMajor && minor >= wantMinor)
}

// Strip1M removes the client-only [1m] marker; no upstream accepts it on the wire.
func Strip1M(id string) string {
	for Has1M(id) {
		id = id[:len(id)-len(marker1M)]
	}
	return id
}

// Has1M reports a trailing client-only [1m] marker, any case.
func Has1M(id string) bool {
	return len(id) >= len(marker1M) && strings.EqualFold(id[len(id)-len(marker1M):], marker1M)
}

// Keeps1M reports a [1m] marker that matters: only opus/sonnet 4.x opt in; Claude 5+ is 1M by default, others never.
func Keeps1M(id string) bool {
	if !Has1M(id) {
		return false
	}
	family, major, _, ok := ParseClaudeID(id)
	return ok && major == 4 && (family == Opus || family == Sonnet)
}

func isFamily(s string) bool { return s == Opus || s == Sonnet || s == Haiku || s == Fable }

func hasFamilyDash(s string) bool {
	i := strings.IndexByte(s, '-')
	return i > 0 && isFamily(s[:i])
}

// Canonical reduces a typed model id to the plain Claude id and reports a trailing [1m]; non-Claude ids pass through.
func Canonical(id string) (base string, oneM bool) {
	id = strings.TrimSpace(id)
	marked := Has1M(id)
	oneM = Keeps1M(id)
	id = Strip1M(id)
	if strings.Contains(id, "/") {
		return id, oneM
	}
	rest, stripped := stripWirePrefix(id)
	cand := rest
	if hasFamilyDash(rest) {
		cand = "claude-" + rest
	}
	if stripped || cand != rest {
		if _, _, _, ok := ParseClaudeID(cand); ok {
			return cand, marked && Keeps1M(cand+marker1M)
		}
	}
	return id, oneM
}

// Window is the model's own context window
func Window(id string) int {
	if l, ok := Limits(id); ok && l.Context > 0 {
		return l.Context
	}
	if Keeps1M(id) {
		return ContextWindow1M
	}
	if canonical, _, ok := Resolve(id); ok && FamilyOf(canonical) != "" {
		if _, major, _, _ := ParseClaudeID(canonical); major >= 5 {
			return ContextWindow1M
		}
	}
	if id != "" && FamilyKind(id) != FamilyClaude {
		return ContextWindowUnknown
	}
	return ContextWindowStd
}

// PromptTokens is the prompt budget in a window: window - output - 10%, or 0 when output fills it.
func PromptTokens(window, output int) int {
	if window <= output {
		return 0
	}
	return max(0, window-output-window/10)
}

// ThinkingKnobs returns thinking budget and adaptive-disable flag; adaptive-only models reject a budget.
func ThinkingKnobs(id string) (maxTokens, disableAdaptive string) {
	f, maj, min, ok := ParseClaudeID(id)
	if !ok || (f != Haiku && atLeast(maj, min, 4, 8)) {
		return "", ""
	}
	if f == Opus && maj == 4 {
		return opus4Knobs(id, min)
	}
	return thinkingStd, ""
}

// opus4Knobs: Opus 4.7 at 1M takes the high budget; Opus 4.6 takes the standard one with adaptive thinking off.
func opus4Knobs(id string, min int) (maxTokens, disableAdaptive string) {
	if min == 7 && Window(id) == ContextWindow1M {
		return thinkingHigh, ""
	}
	if min == 6 {
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
	familyOther  Family = "other"
)

// legacyClaudeRE matches uncatalogued Claude ids, including claude-3-7-sonnet and Bedrock prefixes.
var legacyClaudeRE = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?:^|[/.:-])claude-`) })

// openAIReasoningRE: OpenAI reasoning models (o-series, gpt-5 and newer) reject max_tokens, temperature and top_p.
var openAIReasoningRE = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^(?:o\d|gpt-(?:[5-9]|[1-9]\d))(?:\D|$)`) })

// normID resolves catalog ids, else lowers, strips [1m] and removes vendor/wire prefixes.
func normID(s string) string {
	if canonical, _, ok := Resolve(s); ok {
		return canonical
	}
	s = Strip1M(strings.ToLower(strings.TrimSpace(s)))
	if _, after, ok := strings.CutLast(s, "/"); ok {
		s = after
	}
	rest, _ := stripWirePrefix(s)
	return rest
}

// FamilyKind classifies vendor families through the catalog, falling back to names for unlisted ids.
func FamilyKind(id string) Family {
	n := normID(id)
	switch {
	case legacyClaudeRE().MatchString(n):
		return FamilyClaude
	case strings.HasPrefix(n, "gpt-") || openAIReasoningRE().MatchString(n):
		return FamilyGPT
	case strings.HasPrefix(n, "gemini-"):
		return FamilyGemini
	}
	return familyOther
}

// IsOpenAIReasoning reports an OpenAI reasoning model (o-series, gpt-5 and newer) under any spelling or vendor prefix.
func IsOpenAIReasoning(id string) bool { return openAIReasoningRE().MatchString(normID(id)) }

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

// Use-case labels, keyed to Anthropic's own alias wording on the Claude model comparison page.
const (
	useFrontier  = "Frontier reasoning" // demanding reasoning and long-horizon agentic work
	useAgentic   = "Agentic coding"     // long-running agentic coding and knowledge work
	useBalanced  = "Balanced"           // best combination of speed and intelligence
	useHighVol   = "High volume"        // high-volume, latency-sensitive: classification, extraction, routing
	useAuto      = "Auto"
	useSpeech    = "Speech"
	useEmbedding = "Embeddings"
	useImage     = "Image"
	useVideo     = "Video"
	useGeneral   = "General"
)

// UseCase maps id/name markers to Claude aliases or non-chat kinds; vendors expose no alias metadata.
func UseCase(id, name string) string {
	s := strings.ToLower(id + " " + name)
	if id == "auto" {
		return useAuto
	}
	for _, k := range kindKeywords {
		if textutil.ContainsAny(s, k.words...) {
			return k.use
		}
	}
	if use, ok := familyUse[FamilyOf(strings.ToLower(id))]; ok {
		return use
	}
	switch {
	case textutil.ContainsAny(s, "gemini", "gemma"):
		return geminiUseCase(s)
	case textutil.ContainsAny(s, "gpt", "o1", "o3", "o4"):
		return gptUseCase(s)
	default:
		return useGeneral
	}
}

// kindKeywords lists the non-chat kinds in match order; the first group whose marker appears in the id or name wins.
var kindKeywords = []struct {
	use   string
	words []string
}{
	{useSpeech, []string{"whisper", "tts", "speech", "audio"}},
	{useEmbedding, []string{"embed"}},
	{useImage, []string{"dall-e", "dalle", "imagen", "image-gen", "-image"}},
	{useVideo, []string{"video", "veo"}},
}

var familyUse = map[string]string{Fable: useFrontier, Opus: useAgentic, Sonnet: useBalanced, Haiku: useHighVol}

func geminiUseCase(s string) string {
	switch {
	case textutil.ContainsAny(s, "flash-lite", "gemma", "nano"):
		return useHighVol
	case textutil.ContainsAny(s, "-pro"):
		return useAgentic
	case textutil.ContainsAny(s, "-flash"):
		return useBalanced
	default:
		return useGeneral
	}
}

func gptUseCase(s string) string {
	switch {
	case textutil.ContainsAny(s, "mini", "nano"):
		return useHighVol
	case textutil.ContainsAny(s, "o1", "o3", "o4") || textutil.ContainsAny(s, "-pro", "high"):
		return useFrontier
	default:
		return useAgentic
	}
}

// Effort tiers; router.EffortTierSignal returns them.
const (
	EffortLow    = "low"
	EffortMedium = "medium"
	EffortHigh   = "high"
)

// AliasModel is the Agent-tool alias for an effort tier ("" for an unknown tier); an alias, so `models --pin` applies.
func AliasModel(alias string) string {
	switch alias {
	case EffortLow:
		return Haiku
	case EffortMedium, EffortHigh:
		return Sonnet
	}
	return ""
}

// Type is the agent type a row, label and receipt show: a fixed name, or else the model's family name.
type Type string

const (
	TypeClaude  Type = "claude"
	TypeCodex   Type = "codex"
	TypeCopilot Type = "copilot"
	TypeGemini  Type = "gemini"
	// TypeCoworker is a Pi code-mode run, shown "coworker".
	TypeCoworker Type = "coworker"
)

// openWeightsGPT is the one GPT-prefixed family that is not OpenAI's GPT and keeps its own name.
const openWeightsGPT = "gpt-oss"

// AgentType is code mode, a GitHub Copilot route, else the model's family; "" when none (the caller's agent then).
func AgentType(routeProvider, model string, codeMode bool) Type {
	switch {
	case codeMode:
		return TypeCoworker
	case routeProvider == "github" || routeProvider == "copilot":
		return TypeCopilot
	}
	switch FamilyKind(model) {
	case FamilyClaude:
		return TypeClaude
	case FamilyGemini:
		return TypeGemini
	case FamilyGPT:
		if !strings.HasPrefix(normID(model), openWeightsGPT) {
			return TypeCodex
		}
	}
	return Type(FamilyName(model))
}

// FamilyName is the id's family: the catalog's, else the first token; "" for an empty id or a bare version.
func FamilyName(id string) string {
	if canonical, _, ok := Resolve(id); ok {
		if f := load().byID[canonical].Family; f != "" {
			return f
		}
	}
	n := strings.TrimPrefix(normID(id), "meta-")
	if strings.HasPrefix(n, openWeightsGPT) {
		return openWeightsGPT
	}
	if i := strings.IndexFunc(n, func(r rune) bool { return r < 'a' || r > 'z' }); i >= 0 {
		n = n[:i]
	}
	return n
}

// ModelSetting is one Claude Code `modelSettings` entry for each alias in Models; an empty field is left unset.
type ModelSetting struct {
	Models            []string `json:"-"`                           // aliases sharing these values
	EffortLevel       string   `json:"effortLevel,omitempty"`       // low|medium|high|xhigh
	MaxEffortLevel    string   `json:"maxEffortLevel,omitempty"`    // low|medium|high|xhigh|max
	AutoCompactWindow any      `json:"autoCompactWindow,omitempty"` // int >= 100000 or "auto"
}

// ModelSettings is the per-model Claude Code settings; launch writes them under the session's Claude model ids.
var ModelSettings = []ModelSetting{
	{Models: []string{Opus, Sonnet, Haiku, Fable}, EffortLevel: "medium"},
}
