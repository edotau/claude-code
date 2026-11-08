// Package agent runs one headless task on an agent (claude, codex, gemini, opencode, copilot)
// under one retry loop; every agent returns the same Result envelope.
package agent

import (
	"context"
	"errors"
	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
	"time"

	"github.com/edotau/claude-code/internal/agent/pack"
)

// RunOptions is one task. Model may be a model alias, a model id, or provider:model.
type RunOptions struct {
	Prompt, Model, Provider, Effort, WorkDir, SessionID string
	Attach                                              []string    // absolute file paths, task text precedes them
	KeepEnv                                             []string    // names the credential scrub must not drop, from --keep-env
	Tools, Autonomous, EnableMemory, Direct             bool        // Direct: agent keeps the provider route, skips the router
	Fleet                                               bool        // copilot only: run in fleet mode (parallel subagents)
	NoTools                                             bool        // claude only: answer from the provider directly, no claude process
	ReadOnly                                            bool        // tools may read but never write, run shell or commit
	NoPersist                                           bool        // claude, codex: no session persistence
	SafeMode                                            bool        // claude, codex, opencode: no hooks/plugins/customizations
	DebugFile                                           string      // claude, gemini: debug log at <path>
	SystemPrompt, AppendSystemPrompt                    string      // claude only: replace / extend the system prompt
	Env                                                 []string    `json:"-"`                   // KEY=VALUE for the child, layered last; never logged
	EnvNames                                            []string    `json:"env_names,omitempty"` // the names of Env, all the dispatch log keeps
	Unset                                               []string    `json:"-"`                   // names dropped from the child's inherited env (an overlay's unset); never logged
	OverlaySet                                          []string    `json:"-"`                   // names an overlay file sets; refused where the route sets them
	OutputSchema                                        string      // JSON Schema text the answer must satisfy
	Permissions                                         Permissions `json:"permissions,omitzero"` // allow/ask/deny rules and mode; zero value = no policy
	Identity                                            string      `json:"identity,omitempty"`   // --as user|sp, claude only: the identity a live holder applies
	Persona                                             *Schema     `json:"persona,omitempty"`    // claude: native agent definition; others: emulated
	Pack                                                *pack.Spec  `json:"pack,omitempty"`       // inline workspace files into Prompt at Prepare; the record keeps the spec, not the bytes
	Timeout                                             time.Duration
	Launcher                                            Launcher           `json:"-"` // claude only: resolves the launch plan
	Agent                                               string             `json:"-"`
	Agents                                              map[string]*Schema `json:"-"`
	Fallback                                            string             `json:"-"`
	Native                                              bool               `json:"-"`
	Host                                                string             `json:"-"`
	RemoteBin                                           string             `json:"-"`
	Rubric                                              string             `json:"-"`
	Background                                          bool               `json:"-"`
}

// statusOf maps the classified failure to the envelope status: quota, deadline, output-then-failure, else error.
func statusOf(e *retry.Error) string {
	switch {
	case e == nil:
		return wire.StatusOK
	case e.Kind == retry.KindQuota:
		return wire.StatusQuota
	case errors.Is(e, context.DeadlineExceeded), errors.Is(e, ErrStalled):
		return wire.StatusStalled
	case e.Progressed:
		return wire.StatusPartial
	}
	return wire.StatusError
}
