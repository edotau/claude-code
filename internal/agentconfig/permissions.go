// Package agentconfig is each agent CLI's spec; this header indexes every agent's allow/deny policy source.
//
//   - Claude Code: internal/settings/template.json `permissions` (ask/deny), seeded into a missing settings.json.
//   - gemini: Claude's ask/deny carried over as GuardRules (templateGuardRules), opted in by
//     agents.json `useGuardRules`; converted in internal/gemini (agyGuardRules).
//   - codex, copilot, opencode launch knobs: agents.json (approvalPolicy, sandboxMode, launch args).
//   - safety-scan hook: internal/hooks/safety*.go blocks destructive shapes for every agent, on top of all of the above.
package agentconfig

// GuardRules are prompts carried over into an agent opted in via Permission.UseGuardRules.
type GuardRules struct {
	Ask  []string `json:"ask"`
	Deny []string `json:"deny"`
}

// Permission is one agent's approval policy: agent-native knobs only that agent's renderer consumes.
type Permission struct {
	UseGuardRules         bool   `json:"useGuardRules,omitempty"`
	ApprovalPolicy        string `json:"approvalPolicy,omitempty"`
	SandboxMode           string `json:"sandboxMode,omitempty"`
	DefaultPermissionMode string `json:"defaultPermissionMode,omitempty"`
	Defaults              Policy `json:"defaults,omitzero"` // the agent's default dispatch policy (agent.DefaultPermissions)
}

// Extras keys the legacy Permission knobs map to in a vault.AgentRecord.
const (
	extraApprovalPolicy        = "approvalPolicy"
	extraSandboxMode           = "sandboxMode"
	extraDefaultPermissionMode = "defaultPermissionMode"
	extraUseGuardRules         = "useGuardRules"
)

// Policy is a dispatch policy in Claude Code's rule grammar, e.g. deny ["Bash(rm:*)"]; mode default, auto or plan.
type Policy struct {
	Mode  string   `json:"mode,omitempty"`
	Allow []string `json:"allow,omitempty"`
	Ask   []string `json:"ask,omitempty"`
	Deny  []string `json:"deny,omitempty"`
}
