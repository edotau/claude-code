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
	nativeEfforts []string
}

// roster is the ordered agent names and their routing facts.
var roster = []struct {
	name string
	RouteFacts
}{
	{Claude, RouteFacts{AnthropicRoute: true, ModelFamily: "claude", nativeEfforts: EffortNames()}},
	{Codex, RouteFacts{ResponsesRoute: true, nativeEfforts: []string{"minimal", "low", "medium", "high", "xhigh"}}},
	{Copilot, RouteFacts{FreezeTarget: true, Discovery: true}},
	{Gemini, RouteFacts{EffortSuffix: true, ModelFamily: "gemini", binary: "agy", GoogleOrDatabricks: true, NoWorkspaceFallback: true, nativeEfforts: EffortNames()}},
	{OpenCode, RouteFacts{SubLadder: true, Discovery: true}},
	{Pi, RouteFacts{Discovery: true, NoOwnLogin: true, NoProjection: true, nativeEfforts: []string{"minimal", "low", "medium", "high", "xhigh", "max"}}},
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
