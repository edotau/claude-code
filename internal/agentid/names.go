// Package agentid holds the canonical roster agent names and the harness reasoning-effort scale.
package agentid

import "slices"

// The five roster agents.
const (
	Claude   = "claude"
	Codex    = "codex"
	Copilot  = "copilot"
	Gemini   = "gemini"
	OpenCode = "opencode"
)

// All is every roster agent: claude first, then the external CLIs by name; a fresh slice each call.
func All() []string { return []string{Claude, Codex, Copilot, Gemini, OpenCode} }

// External is the agents that run an external CLI (All without claude).
func External() []string { return All()[1:] }

// Known reports whether name is a roster agent.
func Known(name string) bool { return slices.Contains(All(), name) }

// Binary is the executable an agent runs: gemini is agy, every other agent its own name.
func Binary(name string) string {
	if name == Gemini {
		return "agy"
	}
	return name
}
