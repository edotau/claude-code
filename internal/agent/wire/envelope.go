// Package wire is the dispatch envelope: events, Result, NDJSON framing, the log tee and the structured-answer check.
package wire

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/textutil"
)

// EnvelopeSchema versions the `ask --format json` envelope.
const EnvelopeSchema = 1

// EnvelopeError is the envelope's error object: a stable kind token plus the redacted message.
type EnvelopeError struct {
	Kind    string `json:"kind"` // auth | rate_limit | transient | fatal | quota | timeout | canceled | usage | schema
	Message string `json:"message"`
}

// Envelope is the one typed JSON body `ask --format json` prints for every agent, on success and on every error path.
type Envelope struct {
	Schema          int             `json:"schema"`
	Status          string          `json:"status"` // ok | error, refined to partial | quota | stalled on a failed run
	Agent           string          `json:"agent"`
	Provider        string          `json:"provider"` // the profile that answered
	Model           string          `json:"model"`
	Invoked         string          `json:"invoked_model"`
	DispatchID      string          `json:"dispatch_id"` // the id `<agent> status|result` and `ask --retry` accept; "" before a log record exists
	SessionID       string          `json:"session_id"`
	Fallback        *string         `json:"fallback"` // null unless a different agent or route than requested ran
	FallbackReason  string          `json:"fallback_reason"`
	Usage           *Usage          `json:"usage"` // null when the agent reports no token counts
	DurationMs      int64           `json:"duration_ms"`
	Error           *EnvelopeError  `json:"error"` // null on success
	Answer          string          `json:"answer"`
	Dropped         []string        `json:"dropped"`
	SchemaUnchecked []string        `json:"schema_unchecked,omitempty"` // --output-schema keywords structured was not checked against
	Emulated        []string        `json:"emulated,omitempty"`         // flags the harness shimmed because the agent has no channel
	Handoff         string          `json:"handoff"`
	Transport       *Transport      `json:"transport"`
	TaskID          string          `json:"task_id,omitempty"`    // ask --batch: the task's id
	BatchID         string          `json:"batch_id,omitempty"`   // ask --batch: the id shared by every task of the run
	Hops            []Hop           `json:"hops,omitempty"`       // every dispatch tried, in order
	RanOn           string          `json:"ran_on,omitempty"`     // the one-line "ran on" summary of the dispatch that answered
	Structured      json.RawMessage `json:"structured,omitempty"` // --output-schema: the answer as JSON satisfying the schema
}

// NewEnvelope builds the envelope for a finished run; err nil means success.
func NewEnvelope(res Result, err error, dispatchID string) Envelope {
	env := Envelope{Schema: EnvelopeSchema, Status: res.Status, Agent: res.Agent, Provider: res.Provider, Model: res.Model,
		Invoked: res.Invoked, DispatchID: dispatchID, SessionID: res.SessionID, FallbackReason: res.FallbackReason,
		DurationMs: res.DurationMs, Answer: res.Answer, Dropped: res.Dropped, Emulated: res.Emulated, Handoff: res.Handoff, Transport: res.Transport, Hops: res.Hops, RanOn: RanOn(res), Structured: res.Structured, SchemaUnchecked: res.SchemaUnchecked}
	if env.Dropped == nil {
		env.Dropped = []string{}
	}
	if res.Fallback != "" {
		env.Fallback = &res.Fallback
	}
	if res.Usage != (Usage{}) {
		u := res.Usage
		env.Usage = &u
	}
	if err != nil {
		env.Error = &EnvelopeError{Kind: ErrorKind(err), Message: textutil.RedactSecrets(err.Error())}
	}
	if env.Status == "" {
		env.Status = StatusOK
		if err != nil {
			env.Status = StatusError
		}
	}
	return env
}

// FailureEnvelope is the envelope for a request that failed before any run (unknown agent or profile, bad flag).
func FailureEnvelope(agentName string, err error) Envelope {
	env := NewEnvelope(Result{Agent: agentName, Status: StatusError}, err, "")
	if env.Error != nil && env.Error.Kind == "fatal" {
		env.Error.Kind = "usage"
	}
	return env
}

var envelopeKindTokens = map[retry.Kind]string{
	retry.KindAuth: "auth", retry.KindRateLimit: "rate_limit", retry.KindTransient: "transient", retry.KindQuota: "quota",
}

// KindOf is the failure class of an envelope error token, the inverse of ErrorKind's table; unknown is fatal.
func KindOf(token string) retry.Kind {
	for k, t := range envelopeKindTokens {
		if t == token {
			return k
		}
	}
	return retry.KindFatal
}

// ErrorKind is the envelope token for err's class; an unclassified error is fatal.
func ErrorKind(err error) string {
	if re, ok := errors.AsType[RemoteError](err); ok {
		return re.Kind
	}
	switch {
	case errors.Is(err, errSchema):
		return "schema"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	if e, ok := errors.AsType[*retry.Error](err); ok {
		if e.AuthReason != "" { // a condemned login is an auth failure, whatever the failing dispatch classified it as
			return "auth"
		}
		if token := envelopeKindTokens[e.Kind]; token != "" {
			return token
		}
	}
	return "fatal"
}

// RanOn is the "ran on" line for a finished run, from its final hop: agent · account·profile · model · fallback.
func RanOn(res Result) string {
	if len(res.Hops) == 0 && res.Status == StatusError { // no dispatch started: nothing ran
		return ""
	}
	who, acct, model := finalHop(res)
	if who == "" {
		return ""
	}
	fb := "none"
	if res.Fallback != "" {
		fb = "from " + res.Fallback
	}
	return fmt.Sprintf("ran on %s · %s · %s · fallback %s", who, cmp.Or(acct, "own login"), cmp.Or(model, "default"), fb) +
		ranOnCost(res)
}

// finalHop is the agent, account label and model of a run's last hop, falling back to the result's own.
func finalHop(res Result) (who, acct, model string) {
	who, model = res.Agent, res.Model
	n := len(res.Hops)
	if n == 0 {
		return who, "", model
	}
	h := res.Hops[n-1]
	who, model, acct = cmp.Or(h.Agent, who), cmp.Or(h.Model, model), h.Account
	if h.Profile != "" && !strings.HasSuffix(acct, h.Profile) { // the account label may already name the profile
		acct = strings.Join(slices.DeleteFunc([]string{acct, h.Profile}, func(s string) bool { return s == "" }), "·")
	}
	return who, acct, model
}

// ranOnCost is " · <tokens> tok · <time>" for what the run reported; a missing measure is left out.
func ranOnCost(res Result) string {
	var b strings.Builder
	u := res.Usage
	if n := u.InputTokens + u.CacheCreationTokens + u.CacheReadTokens + u.OutputTokens; n > 0 {
		b.WriteString(" · ")
		b.WriteString(textutil.CompactCount(int64(n)))
		b.WriteString(" tok")
	}
	if res.DurationMs > 0 {
		b.WriteString(" · ")
		b.WriteString((time.Duration(res.DurationMs) * time.Millisecond).Round(time.Second).String())
	}
	return b.String()
}

// DidNotRun is the "did not run" line for a dispatch that ended before any dispatch started, else "".
func DidNotRun(res Result) string {
	if len(res.Hops) > 0 || res.Status != StatusError || res.Agent == "" {
		return ""
	}
	return strings.TrimSpace("did not run " + res.Agent + " — " + strings.TrimPrefix(res.FallbackReason, res.Agent+": "))
}

// RemoteError is a failure the remote reported in its envelope; Kind is the remote's envelope token, kept verbatim.
type RemoteError struct{ Kind, Msg string }

func (e RemoteError) Error() string { return e.Msg }

const (
	LiveSchema     = 1
	LiveSockSuffix = ".sock"
)

// LiveRequest is the one line a client sends on the session socket: a single user turn.
type LiveRequest struct {
	Schema int    `json:"schema"`
	Prompt string `json:"prompt"`
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

// Result is one run's outcome; NewEnvelope turns it into the Envelope that `ask --format json` prints.
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
	Fallback        string          `json:"fallback,omitempty"`        // the requested agent when another agent or route answered; Agent is the answerer
	FallbackReason  string          `json:"fallback_reason,omitempty"` // why the original agent was unavailable
	Handoff         string          `json:"handoff,omitempty"`         // hand-off file written when a copilot dispatch hit its quota
	Transport       *Transport      `json:"transport,omitempty"`       // stream health; fields an agent can't report stay absent
	Hops            []Hop           `json:"hops,omitempty"`            // every dispatch runWithFallback tried, in order
	Structured      json.RawMessage `json:"-"`                         // the answer as JSON satisfying RunOptions.OutputSchema
	Recovered       []*retry.Error  `json:"-"`                         // attempts that failed before the successful one, for the hop's errors
}

// Hop is one dispatch runWithFallback tried; ErrorKind is the envelope error token, empty when the dispatch answered.
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
