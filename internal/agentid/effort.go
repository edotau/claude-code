package agentid

import (
	"fmt"
	"slices"
	"strings"
)

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

// Efforts is the harness scale, weakest first.
func Efforts() []Effort { return []Effort{EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax} }

// EffortNames is Efforts as strings, for flag help and enum checks.
func EffortNames() []string {
	names := make([]string, 0, len(Efforts()))
	for _, e := range Efforts() {
		names = append(names, string(e))
	}
	return names
}

// NativeEfforts is the values agent's CLI accepts, weakest first; nil means model-specific (passed through).
func NativeEfforts(agent string) []string {
	switch agent {
	case Claude, Gemini:
		return EffortNames()
	case Codex:
		return []string{"minimal", "low", "medium", "high", "xhigh"}
	}
	return nil
}

// NativeEffort spells e for agent's CLI; clamped reports a move to the nearest value the CLI has; "" means refused.
func NativeEffort(agent string, e Effort) (native string, clamped bool) {
	if !Known(agent) {
		return "", false
	}
	natives := NativeEfforts(agent)
	if slices.Contains(natives, string(e)) {
		return string(e), false
	}
	i := slices.Index(Efforts(), e)
	if i < 0 {
		return "", false
	}
	if natives == nil {
		return string(e), false
	}
	for _, weaker := range slices.Backward(Efforts()[:i]) {
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
