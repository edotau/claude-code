package wire

import (
	"encoding/json"
	"github.com/edotau/claude-code/internal/agent/retry"
)

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
	Recovered       []*retry.Error  // attempts that failed before the successful one, for the hop's errors
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
