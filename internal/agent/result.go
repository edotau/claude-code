// Package agent runs one headless task on an agent (claude, codex, gemini, opencode, copilot)
// under one retry loop; every agent returns the same Result envelope.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/edotau/claude-code/internal/agent/retry"
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

// Usage is token accounting where the agent reports it.
type Usage struct {
	InputTokens         int `json:"input_tokens"`                    // uncached input only
	CacheCreationTokens int `json:"cache_creation_tokens,omitempty"` // billed above input; kept apart so cost can be priced
	CacheReadTokens     int `json:"cache_read_tokens,omitempty"`
	OutputTokens        int `json:"output_tokens"`
	// CostUSD is the agent-reported cost where it reports one (claude, agy); absent for the rest.
	CostUSD float64 `json:"cost_usd,omitempty"`
}

// Result is the envelope `ask --format json` prints.
type Result struct {
	DispatchID      string          `json:"-"`
	Agent           string          `json:"agent"`
	Status          string          `json:"status"` // ok | error | partial | quota | stalled
	Provider        string          `json:"provider,omitempty"`
	Model           string          `json:"model,omitempty"`
	Invoked         string          `json:"invoked_model,omitempty"` // the model (<profile>:<model> when pinned) the run was asked for; not upstream-confirmed
	SessionID       string          `json:"session_id,omitempty"`
	Answer          string          `json:"answer"`
	Usage           Usage           `json:"usage"`
	DurationMs      int64           `json:"duration_ms"`
	Dropped         []string        `json:"dropped,omitempty"`
	SchemaUnchecked []string        `json:"-"`                         // schema keywords the minimal validator does not enforce
	Emulated        []string        `json:"emulated,omitempty"`        // flags the harness shimmed because the agent has no channel
	Fallback        string          `json:"fallback,omitempty"`        // set to the original agent's name when it ran instead
	FallbackReason  string          `json:"fallback_reason,omitempty"` // why the original agent was unavailable
	Handoff         string          `json:"handoff,omitempty"`         // hand-off file written when a copilot dispatch hit its quota
	Transport       *Transport      `json:"transport,omitempty"`       // stream health; fields an agent can't report stay absent
	Hops            []Hop           `json:"hops,omitempty"`            // every dispatch RunWithFallback tried, in order
	Structured      json.RawMessage `json:"-"`                         // the answer as JSON satisfying RunOptions.OutputSchema
	recovered       []*retry.Error  // attempts that failed before the successful one, for the hop's errors
}

// Hop is one dispatch RunWithFallback tried; ErrorKind is the envelope error token, empty when the dispatch answered.
type Hop struct {
	Agent     string `json:"agent"`
	Account   string `json:"account,omitempty"`
	Profile   string `json:"profile,omitempty"`
	Model     string `json:"model,omitempty"`
	ErrorKind string `json:"error_kind,omitempty"`
	// Errors lists every attempt's failure, oldest first, when the dispatch was retried.
	Errors []EnvelopeError `json:"errors,omitempty"`
}

// Transport is a run's stream health: attempts, retries the agent recovered from, and how steady the stream was.
type Transport struct {
	Attempts      int   `json:"attempts"`                 // tries the outer retry loop made
	Retries       int   `json:"retries,omitempty"`        // api_retry events Claude recovered from
	RetryStatuses []int `json:"retry_statuses,omitempty"` // their HTTP status; 0 = no response
	FirstEventMs  int64 `json:"first_event_ms,omitempty"` // start → first stream_event
	MaxGapMs      int64 `json:"max_gap_ms,omitempty"`     // longest silence between stream_events
}

// Result.Status values.
const (
	StatusOK      = "ok"
	StatusError   = "error"
	StatusPartial = "partial"
	StatusQuota   = "quota"
	StatusStalled = "stalled"
)

// statusOf maps the classified failure to the envelope status: quota, deadline, output-then-failure, else error.
func statusOf(e *retry.Error) string {
	switch {
	case e == nil:
		return StatusOK
	case e.Kind == retry.KindQuota:
		return StatusQuota
	case errors.Is(e, context.DeadlineExceeded), errors.Is(e, ErrStalled):
		return StatusStalled
	case e.Progressed:
		return StatusPartial
	}
	return StatusError
}
