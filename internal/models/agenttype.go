package models

import "strings"

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
