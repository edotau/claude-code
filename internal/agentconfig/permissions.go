// Package agentconfig is each agent CLI's spec; this header indexes every agent's allow/deny policy source.
//
//   - Claude Code: internal/settings/template.json `permissions` (ask/deny), seeded into a missing settings.json.
//   - gemini: Claude's ask/deny carried over as GuardRules (templateGuardRules), opted in by
//     agents.json `useGuardRules`; converted in internal/gemini (agyGuardRules).
//   - copilot shell allowlist: agents.json agents.copilot.permission.shell, read here by ShellPolicy() and
//     enforced by internal/shell (SDK --tools tier and the `hook copilot-bash` guard). Argument-level git logic stays Go
//     (shell check "scopedGit"); a rule's "check" names such a Go rule. Shell syntax beyond the rules (cd && chains,
//     Go env prefixes, $TMPDIR in mktemp, 2>&1, </dev/null) is judged in internal/shell/scope.go.
//   - codex, copilot, opencode launch knobs: agents.json (approvalPolicy, sandboxMode, launch args).
//   - safety-scan hook: internal/hooks/safety*.go blocks destructive shapes for every agent, on top of all of the above.
//
// ShellPolicy reads only the embedded agents.json (never the vault overlay), once; a parse failure fails closed.
package agentconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// GuardRules are prompts carried over into an agent opted in via Permission.UseGuardRules.
type GuardRules struct {
	Ask  []string `json:"ask"`
	Deny []string `json:"deny"`
}

// Permission is one agent's approval policy: agent-native knobs only that agent's renderer consumes.
type Permission struct {
	UseGuardRules         bool        `json:"useGuardRules,omitempty"`
	ApprovalPolicy        string      `json:"approvalPolicy,omitempty"`
	SandboxMode           string      `json:"sandboxMode,omitempty"`
	DefaultPermissionMode string      `json:"defaultPermissionMode,omitempty"`
	Shell                 *ShellScope `json:"shell,omitempty"`   // guards read the embedded copy only (ShellPolicy)
	Defaults              Policy      `json:"defaults,omitzero"` // the agent's default dispatch policy (agent.DefaultPermissions)
}

// Policy is a dispatch policy in Claude Code's rule grammar, e.g. deny ["Bash(rm:*)"]; mode default, auto or plan.
type Policy struct {
	Mode  string   `json:"mode,omitempty"`
	Allow []string `json:"allow,omitempty"`
	Ask   []string `json:"ask,omitempty"`
	Deny  []string `json:"deny,omitempty"`
}

// ShellRule admits one command family; with no subcommands, exact or targets it admits any args.
type ShellRule struct {
	Commands    []string `json:"commands"`
	Subcommands []string `json:"subcommands,omitempty"` // args start with one of these (space-separated words)
	Exact       []string `json:"exact,omitempty"`       // or args equal one of these exactly
	Targets     []string `json:"targets,omitempty"`     // every arg, at least one, is one of these
	BannedFlags []string `json:"bannedFlags,omitempty"` // no arg is one of these, alone or as flag=value
	ArgsInside  bool     `json:"argsInside,omitempty"`  // absolute and .. args must resolve inside the workdir
	Check       string   `json:"check,omitempty"`       // a named Go rule in shell, run on the args after the match
	Summary     string   `json:"summary,omitempty"`     // prose, replacing the generated one (required with a check)
}

// ShellRules is an ordered rule list; order drives Summary.
type ShellRules []ShellRule

// ShellScope is copilot's scoped shell: Commands everywhere, HookExtras for the hook only.
type ShellScope struct {
	Commands   ShellRules `json:"commands"`
	HookExtras ShellRules `json:"hookExtras"`
}

var shellPolicy = sync.OnceValue(func() ShellScope {
	p, err := parseShellPolicy(defaultsJSON)
	if err != nil {
		return ShellScope{} // fail closed: no rule admits anything
	}
	return p
})

// ShellPolicy is the embedded copilot shell policy, parsed once; the zero value (nothing allowed) on a parse failure.
func ShellPolicy() ShellScope { return shellPolicy() }

// parseShellPolicy strictly decodes agents.copilot.permission.shell: an unknown key is an error, never ignored.
func parseShellPolicy(doc []byte) (ShellScope, error) {
	var outer struct {
		Agents map[string]struct {
			Permission struct {
				Shell json.RawMessage `json:"shell"`
			} `json:"permission"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(doc, &outer); err != nil {
		return ShellScope{}, fmt.Errorf("agents.json: %w", err)
	}
	raw := outer.Agents["copilot"].Permission.Shell
	if len(raw) == 0 {
		return ShellScope{}, errors.New("agents.json: no agents.copilot.permission.shell")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var p ShellScope
	if err := dec.Decode(&p); err != nil {
		return ShellScope{}, fmt.Errorf("agents.json copilot shell: %w", err)
	}
	return p, nil
}

// Summary is the rules as prose, e.g. "go test|vet, gofmt, ls cat head".
func (rs ShellRules) Summary() string {
	items := make([]string, len(rs))
	for i, r := range rs {
		items[i] = r.Summary
		if items[i] == "" {
			items[i] = strings.Join(r.Commands, " ")
			if subs := r.args(); len(subs) > 0 {
				items[i] += " " + strings.Join(subs, "|")
			}
		}
	}
	return strings.Join(items, ", ")
}

// args lists the rule's admitted leading args in policy order.
func (r ShellRule) args() []string {
	return append(append(append([]string(nil), r.Subcommands...), r.Exact...), r.Targets...)
}
