package models

import "strings"

// Type is the agent type a row, label and receipt show: a fixed name, or else the model's family name.
type Type string

const (
	TypeClaude  Type = "claude"
	TypeCodex   Type = "codex"
	TypeCopilot Type = "copilot"
	TypeGemini  Type = "gemini"
	TypePi      Type = "pi"
)

// openWeightsGPT is the one GPT-prefixed family that is not OpenAI's GPT and keeps its own name.
const openWeightsGPT = "gpt-oss"

// AgentType is total: code mode, a GitHub Copilot route, the model's family; never empty for a non-empty model.
func AgentType(routeProvider, model string, codeMode bool) Type {
	switch {
	case codeMode:
		return TypePi
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

// FamilyName is the id's family: the catalog's, else the first token; the runner's own name for an empty id.
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
	if n == "" {
		return string(TypePi)
	}
	return n
}
