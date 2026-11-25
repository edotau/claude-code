// Package agentid holds the canonical roster agent names and the harness reasoning-effort scale.
package agentid

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

// The six roster agents.
const (
	Claude   = "claude"
	Codex    = "codex"
	Copilot  = "copilot"
	Gemini   = "gemini"
	OpenCode = "opencode"
	Pi       = "pi"
)

// All is every roster agent: claude first, then the external CLIs by name; a fresh slice each call.
func All() []string {
	names := make([]string, len(roster))
	for i, agent := range roster {
		names[i] = agent.name
	}
	return names
}

// External is the agents that run an external CLI (All without claude).
func External() []string { return All()[1:] }

// Projected is the external agents whose CLI reads a synced agents dir (External without NoProjection ones).
func Projected() []string {
	return slices.DeleteFunc(External(), func(n string) bool { return facts(n).NoProjection })
}

// CodeModeType is the Claude subagent type that runs Pi in code mode; Pi has no type under its own name.
const CodeModeType = "code-mode"

// SubagentType is the Claude subagent type that dispatches agent: code-mode for Pi, else the agent's own name.
func SubagentType(agent string) string {
	if agent == Pi {
		return CodeModeType
	}
	return agent
}

// AgentOfType is the roster agent a subagent type dispatches; "" for any other type, a bare "pi" included.
func AgentOfType(subagentType string) string {
	for _, a := range All() {
		if SubagentType(a) == subagentType {
			return a
		}
	}
	return ""
}

// Known reports whether name is a roster agent.
func Known(name string) bool { return slices.Contains(All(), name) }

// Binary is the executable an agent runs: gemini is agy, every other agent its own name.
func Binary(name string) string { return cmp.Or(facts(name).binary, name) }

// Effort is a reasoning-effort level on the harness scale.
type Effort string

// The harness effort scale, weakest first.
const (
	EffortLow    Effort = "low"
	EffortMedium Effort = "medium"
	EffortHigh   Effort = "high"
	EffortXHigh  Effort = "xhigh"
	EffortMax    Effort = "max"
)

// efforts is the harness scale, weakest first.
func efforts() []Effort { return []Effort{EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax} }

// EffortNames is the harness efforts as strings, for flag help and enum checks.
func EffortNames() []string {
	names := make([]string, 0, len(efforts()))
	for _, e := range efforts() {
		names = append(names, string(e))
	}
	return names
}

// RouteFacts is what routing needs to know about an agent, so callers test a fact, not a name.
type RouteFacts struct {
	// EffortSuffix encodes native reasoning effort as a trailing model-id component.
	EffortSuffix bool
	// AnthropicRoute accepts unlisted ids and enforces Claude Code profile constraints.
	AnthropicRoute bool
	// FreezeTarget resolves billing once before dispatch guards and retries.
	FreezeTarget                                                                  bool
	ModelFamily, binary                                                           string
	ResponsesRoute, SubLadder, Discovery, GoogleOrDatabricks, NoWorkspaceFallback bool
	// NoOwnLogin runs only on a pinned Databricks workspace: no vendor login row.
	NoOwnLogin bool
	// NoProjection has no vendor home to sync agents into.
	NoProjection  bool
	CodeMode      CodeModeRow
	nativeEfforts []string
}

// CodeModeRow is how an agent's code mode turns on; the zero row means the agent has none.
type CodeModeRow struct {
	// Var is the vendor switch (set to on); empty for an extension-based mode.
	Var string
	// Extension marks a mode the harness loads itself (pi's codemode extension).
	Extension bool
	// Only is true when the agent has a script-only mode.
	Only bool
	// ViaSettings passes Var in the --settings env block, not the process env.
	ViaSettings bool
	// DirectOnly refuses the mode on any gateway or cloud-provider route.
	DirectOnly bool
	// TypedByMode: the agent's type names its code-mode runs; a run with code mode off is typed by its model.
	TypedByMode bool
}

// Has reports whether the agent has a code mode.
func (r CodeModeRow) Has() bool { return r.Var != "" || r.Extension }

// CodeModeAgents is the roster agents with a code-mode row, in roster order.
func CodeModeAgents() []string {
	var names []string
	for _, a := range roster {
		if a.CodeMode.Has() {
			names = append(names, a.name)
		}
	}
	return names
}

// roster is the ordered agent names and their routing facts.
var roster = []struct {
	name string
	RouteFacts
}{
	{Claude, RouteFacts{AnthropicRoute: true, ModelFamily: "claude", nativeEfforts: EffortNames(),
		CodeMode: CodeModeRow{Var: "CLAUDE_CODE_EXPERIMENTAL_CODE_MODE", ViaSettings: true, DirectOnly: true}}},
	{Codex, RouteFacts{ResponsesRoute: true, nativeEfforts: []string{"minimal", "low", "medium", "high", "xhigh"}}},
	{Copilot, RouteFacts{FreezeTarget: true, Discovery: true}},
	{Gemini, RouteFacts{EffortSuffix: true, ModelFamily: "gemini", binary: "agy", GoogleOrDatabricks: true, NoWorkspaceFallback: true, nativeEfforts: EffortNames()}},
	{OpenCode, RouteFacts{SubLadder: true, Discovery: true, CodeMode: CodeModeRow{Var: "OPENCODE_EXPERIMENTAL_CODE_MODE"}}},
	{Pi, RouteFacts{Discovery: true, NoOwnLogin: true, NoProjection: true, CodeMode: CodeModeRow{Extension: true, Only: true, TypedByMode: true},
		nativeEfforts: []string{"minimal", "low", "medium", "high", "xhigh", "max"}}},
}

func facts(name string) RouteFacts {
	for _, agent := range roster {
		if agent.name == name {
			return agent.RouteFacts
		}
	}
	return RouteFacts{}
}

// Routing is name's routing facts.
func Routing(name string) RouteFacts { return facts(name) }

// NativeEfforts is the values agent's CLI accepts, weakest first; nil means model-specific (passed through).
func NativeEfforts(agent string) []string { return slices.Clone(facts(agent).nativeEfforts) }

// SplitNativeEffort cuts a case-sensitive Gemini effort suffix, preserving the split even when ok is false.
func SplitNativeEffort(model string) (base, effort string, ok bool) {
	base, effort, cut := strings.CutLast(model, "-")
	return base, effort, cut && slices.Contains(NativeEfforts(Gemini), effort)
}

// NativeEffort spells e for agent's CLI; clamped reports a move to the nearest value the CLI has; "" means refused.
func NativeEffort(agent string, e Effort) (native string, clamped bool) {
	if !Known(agent) {
		return "", false
	}
	return ClampEffort(NativeEfforts(agent), e)
}

// ClampEffort spells e against supported values, weakest first; nil passes known levels through.
func ClampEffort(natives []string, e Effort) (native string, clamped bool) {
	if slices.Contains(natives, string(e)) {
		return string(e), false
	}
	i := slices.Index(efforts(), e)
	if i < 0 {
		return "", false
	}
	if natives == nil {
		return string(e), false
	}
	for _, weaker := range slices.Backward(efforts()[:i]) {
		if slices.Contains(natives, string(weaker)) {
			return string(weaker), true
		}
	}
	return "", false
}

// ValidEffort accepts "" and any value on the harness scale or in agent's native list.
func ValidEffort(agent, s string) error {
	accepted := EffortNames()
	for _, n := range NativeEfforts(agent) {
		if !slices.Contains(accepted, n) {
			accepted = append(accepted, n)
		}
	}
	if s == "" || slices.Contains(accepted, s) {
		return nil
	}
	return fmt.Errorf("effort %q for %s: want one of %s", s, agent, strings.Join(accepted, ", "))
}
