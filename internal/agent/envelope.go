package agent

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/secrets/shapes"
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
	Hops            []Hop           `json:"hops,omitempty"`       // every leg tried, in order
	RanOn           string          `json:"ran_on,omitempty"`     // the one-line "ran on" summary of the leg that answered
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
		env.Error = &EnvelopeError{Kind: errorKind(err), Message: shapes.Redact(err.Error())}
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

// errorKind is the envelope token for err's class; an unclassified error is fatal.
func errorKind(err error) string {
	if re, ok := errors.AsType[remoteError](err); ok {
		return re.kind
	}
	switch {
	case errors.Is(err, ErrSchema):
		return "schema"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	if e, ok := errors.AsType[*Error](err); ok {
		if e.AuthReason != "" { // a condemned login is an auth failure, whatever the failing leg classified it as
			return "auth"
		}
		switch e.Kind {
		case KindAuth:
			return "auth"
		case KindRateLimit:
			return "rate_limit"
		case KindTransient:
			return "transient"
		case KindQuota:
			return "quota"
		}
	}
	return "fatal"
}

// RanOn is the "ran on" line for a finished run, from its final hop: agent · account·profile · model · fallback.
func RanOn(res Result) string {
	if len(res.Hops) == 0 && res.Status == StatusError { // no leg started: nothing ran
		return ""
	}
	who, acct, model := res.Agent, "", res.Model
	if n := len(res.Hops); n > 0 {
		h := res.Hops[n-1]
		who, model = cmp.Or(h.Agent, who), cmp.Or(h.Model, model)
		acct = h.Account
		if h.Profile != "" && !strings.HasSuffix(acct, h.Profile) { // the account label may already name the profile
			acct = strings.Join(slices.DeleteFunc([]string{acct, h.Profile}, func(s string) bool { return s == "" }), "·")
		}
	}
	if who == "" {
		return ""
	}
	fb := "none"
	if res.Fallback != "" {
		fb = "from " + res.Fallback
	}
	return fmt.Sprintf("ran on %s · %s · %s · fallback %s", who, cmp.Or(acct, "own login"), cmp.Or(model, "default"), fb)
}

// DidNotRun is the "did not run" line for a dispatch that ended before any leg started, else "".
func DidNotRun(res Result) string {
	if len(res.Hops) > 0 || res.Status != StatusError || res.Agent == "" {
		return ""
	}
	return strings.TrimSpace("did not run " + res.Agent + " — " + strings.TrimPrefix(res.FallbackReason, res.Agent+": "))
}
