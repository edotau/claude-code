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
	"github.com/edotau/claude-code/internal/contract"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/textutil"
)

// EnvelopeSchema versions the `ask --format json` envelope.
const EnvelopeSchema = 1

// EnvelopeError is the envelope's error object: a stable kind token plus the redacted message.
type EnvelopeError struct {
	Kind    string `json:"kind"` // auth | rate_limit | transient | fatal | quota | timeout | canceled | usage | schema
	Message string `json:"message"`
}

// Denials counts structured shell-tool refusals only (count, first labels); other refusals are not reported.
type Denials struct {
	Count    int      `json:"count"`
	Commands []string `json:"commands"`
}

// RouteObject parses "provider:profile", a bare provider (the pool, profile auto) or a bare profile as a route
// naming provider and profile; nil when empty or the provider is unknown.
func RouteObject(s string) *route.Route {
	r := route.ParseLegacy(s)
	if r.Provider == "" && r.Profile != "" {
		if reg, err := providers.LoadShared(); err == nil {
			if p, err := reg.Get(r.Profile); err == nil {
				r.Provider = p.Provider
			}
		}
	}
	switch {
	case r.Provider == "":
		return nil
	case r.Profile == "":
		r.Profile = "auto"
	}
	return &r
}

// LaunchedWindow is the context window model runs at on the provider row (a typed [1m] included); 0 without a model.
func LaunchedWindow(provider, model string, typed1M bool) int {
	if model == "" {
		return 0
	}
	if typed1M && !models.Has1M(model) {
		model += "[1m]"
	}
	reg, _ := providers.Load()
	return providers.Window(reg.Row(provider), model)
}

// Envelope is the one typed JSON body `ask --format json` prints for every agent, on success and on every error path.
type Envelope struct {
	Schema          int             `json:"schema"`
	Status          string          `json:"status"` // ok | error, refined to partial | quota | stalled on a failed run
	Agent           string          `json:"agent"`
	Provider        string          `json:"provider"` // the profile that answered
	Model           string          `json:"model"`
	Invoked         string          `json:"invoked_model"`
	DispatchID      string          `json:"dispatch_id"` // the id `agents status|result` and `ask --retry` accept; "" before a log record exists
	SessionID       string          `json:"session_id"`
	Fallback        *string         `json:"fallback"` // null unless a different agent or route than requested ran
	FallbackReason  string          `json:"fallback_reason"`
	FallbackKind    string          `json:"fallback_kind,omitempty"`   // the failure kind of the fallen hop (retry.Kind.String())
	Requested       string          `json:"requested,omitempty"`       // the plain model (else profile) asked for, when another step answered
	RequestedWhy    string          `json:"requested_why,omitempty"`   // why the requested target did not answer
	Route           *route.Route    `json:"route,omitempty"`           // the route that answered, as an object
	RequestedRoute  *route.Route    `json:"requested_route,omitempty"` // the route asked for, as an object
	Usage           *Usage          `json:"usage"`                     // null when the agent reports no token counts
	DurationMs      int64           `json:"duration_ms"`
	Turns           int             `json:"turns,omitempty"`          // model turns the agent reported; absent when it reports none
	ContextWindow   int             `json:"context_window,omitempty"` // tokens of the window the answering model was launched at
	Error           *EnvelopeError  `json:"error"`                    // null on success
	Denials         *Denials        `json:"denials,omitempty"`
	Answer          string          `json:"answer"`
	Dropped         []string        `json:"dropped"`
	SchemaUnchecked []string        `json:"schema_unchecked,omitempty"` // --output-schema keywords structured was not checked against
	Emulated        []string        `json:"emulated,omitempty"`         // flags the harness shimmed because the agent has no channel
	Handoff         string          `json:"handoff"`
	Transport       *Transport      `json:"transport"`
	TaskID          string          `json:"task_id,omitempty"`    // ask --batch: the task's id
	BatchID         string          `json:"batch_id,omitempty"`   // ask --batch: the id shared by every task of the run
	Hops            []Hop           `json:"hops,omitempty"`       // every dispatch tried, in order
	Type            string          `json:"type,omitempty"`       // the agent type of the model that ran (models.AgentType)
	Runner          string          `json:"runner,omitempty"`     // the runner that carried Agent's run, "pi"; absent when Agent ran itself
	RanOn           string          `json:"ran_on,omitempty"`     // the one-line "ran on" summary of the dispatch that answered
	Structured      json.RawMessage `json:"structured,omitempty"` // --output-schema: the answer as JSON satisfying the schema
	Verify          *VerifyResult   `json:"verify,omitempty"`     // --spec verify: what the harness ran after the answer
	Handback        *Handback       `json:"handback,omitempty"`   // the run's typed outcome, from a pi run's handback call
}

// Handback is the typed outcome a run reports; the answer's footer is only its rendering (Footer).
type Handback struct {
	Status  string   `json:"status"` // DONE | DONE_WITH_CONCERNS | NEEDS_CONTEXT | BLOCKED
	Summary string   `json:"summary"`
	Unfixed []string `json:"unfixed"`
	// Tests and Method are empty when the worker did not state them (unmet for a contract).
	Tests  contract.Tests  `json:"tests,omitempty"`
	Method contract.Method `json:"method,omitempty"`
}

// Footer is the verdict line and the one UNFIXED line; "UNFIXED: none" only for DONE with no items.
func (h Handback) Footer() string {
	one := func(s string) string { return strings.Join(strings.Fields(s), " ") } // a newline never forges a line
	var items []string
	for _, s := range h.Unfixed {
		if s = one(s); s != "" && !strings.EqualFold(s, "none") {
			items = append(items, s)
		}
	}
	unfixed := strings.Join(items, "; ")
	switch {
	case unfixed != "":
	case h.Status == "DONE":
		unfixed = "none"
	default:
		unfixed = h.Status // an unfinished or concerned run is never "UNFIXED: none"
	}
	head := h.Status
	if s := one(h.Summary); s != "" {
		head += ": " + s
	}
	return head + "\nUNFIXED: " + unfixed
}

// VerifyResult is what the harness observed running a spec's verify steps; Exit is the first failing step's.
type VerifyResult struct {
	Command string `json:"command"`
	Exit    int    `json:"exit"` // -1 when a step could not start
	Ms      int64  `json:"ms"`
	Tail    string `json:"tail,omitempty"`   // the last bytes of the steps' combined output
	Reason  string `json:"reason,omitempty"` // why the steps did not run: a VerifyNotRun* value
}

// Line is the one-line receipt every output shows: the exit, or why the steps did not run.
func (v VerifyResult) Line() string {
	if v.Reason != "" {
		return fmt.Sprintf("verify: not run (%s): %s", v.Reason, v.Command)
	}
	return fmt.Sprintf("verify: exit %d (%s)", v.Exit, v.Command)
}

// The reasons a verify did not run; the run's own status and exit code are unaffected.
const (
	VerifyNotRunRemote = "remote_workdir"
	VerifyNotRunShape  = "shape"
	// VerifyNotRunUnread is a carried child's result event that could not be read: it may have run verify itself.
	VerifyNotRunUnread = "child_result_unread"
)

// NewEnvelope builds the envelope for a finished run; err nil means success.
func NewEnvelope(res Result, err error, dispatchID string) Envelope {
	env := Envelope{Schema: EnvelopeSchema, Status: res.Status, Agent: res.Agent, Provider: res.Provider, Model: res.Model,
		Invoked: res.Invoked, DispatchID: dispatchID, SessionID: res.SessionID, FallbackReason: res.FallbackReason, Requested: res.Requested, RequestedWhy: res.RequestedWhy,
		DurationMs: res.DurationMs, Turns: res.Turns, ContextWindow: res.ContextWindow, Answer: res.Answer, Dropped: res.Dropped, Emulated: res.Emulated, Handoff: res.Handoff, Transport: res.Transport, Hops: res.Hops, RanOn: RanOn(res), Structured: res.Structured, SchemaUnchecked: res.SchemaUnchecked, Verify: res.Verify, Handback: res.Handback}
	env.Type, env.Runner = res.Type, res.Runner
	env.FallbackKind = res.FallbackKind
	if res.Runner != "" {
		env.Agent = cmp.Or(res.Asked, res.Agent)
	}
	if res.Denials != nil {
		env.Denials = &Denials{Count: res.Denials.Count, Commands: append([]string{}, res.Denials.Commands...)}
	}
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
	env.Route = RouteObject(res.Provider)
	env.RequestedRoute = res.RequestedRoute
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

// RanOn is the "ran on" line for a finished run's final hop: agent · account·profile · model · fallback · proxy.
func RanOn(res Result) string {
	if nothingRan(res) {
		return ""
	}
	who, acct, model := finalHop(res)
	if who == "" {
		return ""
	}
	typ := cmp.Or(res.Type, who)
	line := fmt.Sprintf("ran on %s · %s · %s · fallback %s", RunLabel(typ, res.Runner), cmp.Or(acct, "own login"), cmp.Or(model, "default"), fallbackText(res)) +
		ranOnCost(res)
	if res.Runner != "" && res.Asked != "" && res.Asked != typ {
		line += " · asked " + res.Asked
	}
	if res.Transport != nil && res.Transport.Proxy != "" {
		line += " · proxy " + res.Transport.Proxy
	}
	return line
}

// RunLabel is the label every surface shows for a run: its asked agent type, never the runner that carried it.
func RunLabel(typ, _ string) string { return typ }

// RequestedLabel renders a requested target for a reader: "<model> on <profile>", else whichever is known.
func RequestedLabel(model, profile string) string {
	if model == "" || profile == "" || model == profile {
		return cmp.Or(model, profile)
	}
	return model + " on " + profile
}

// fallbackText is "none", or "from <origin>" naming the requested target and its first reason when known.
func fallbackText(res Result) string {
	if res.Fallback == "" {
		return "none"
	}
	if res.Requested == "" {
		return "from " + res.Fallback
	}
	profile := ""
	if res.RequestedRoute != nil {
		profile = res.RequestedRoute.Profile
	}
	fb := "from " + RequestedLabel(res.Requested, profile)
	first, _, _ := strings.Cut(res.FallbackReason, "; ")
	if why := cmp.Or(res.RequestedWhy, first); why != "" {
		fb += " (" + why + ")"
	}
	return fb
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
	if !nothingRan(res) || res.Agent == "" {
		return ""
	}
	line := "did not run " + res.Agent
	if why := strings.TrimPrefix(res.FallbackReason, res.Agent+": "); why != "" {
		line += " — " + why
	}
	return line
}

// nothingRan reports a failed run with no hop, or one whose every hop was refused before it started.
func nothingRan(res Result) bool {
	if len(res.Hops) == 0 {
		return res.Status == StatusError
	}
	return !slices.ContainsFunc(res.Hops, func(h Hop) bool { return !h.NotStarted })
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
	Invoked         string          `json:"invoked_model,omitempty"` // the plain model id the run was asked for (its profile is Provider); not upstream-confirmed
	Written         []string        `json:"-"`                       // paths the agent's own events proved it wrote
	Reported        string          `json:"-"`                       // the model id the vendor's own stream named; "" when it named none
	SessionID       string          `json:"session_id,omitempty"`
	Denials         *Denials        `json:"denials,omitempty"`
	Answer          string          `json:"answer"`
	Usage           Usage           `json:"usage"`
	DurationMs      int64           `json:"duration_ms"`
	Turns           int             `json:"turns,omitempty"`          // model turns the agent's own stream reported
	ContextWindow   int             `json:"context_window,omitempty"` // the answering model's launched window, set by runWithFallback
	Dropped         []string        `json:"dropped,omitempty"`
	SchemaUnchecked []string        `json:"-"`                         // schema keywords the minimal validator does not enforce
	Emulated        []string        `json:"emulated,omitempty"`        // flags the harness shimmed because the agent has no channel
	Fallback        string          `json:"fallback,omitempty"`        // the requested agent when another agent or route answered; Agent is the answerer
	FallbackReason  string          `json:"fallback_reason,omitempty"` // why the original agent was unavailable
	FallbackKind    string          `json:"fallback_kind,omitempty"`   // retry.Kind.String() of the fallen hop; a string, not the enum, so the wire stays text
	Requested       string          `json:"requested,omitempty"`       // the plain model (else profile) asked for, when another step answered
	RequestedRoute  *route.Route    `json:"requested_route,omitempty"` // the route Requested was asked on
	RequestedWhy    string          `json:"-"`                         // why the requested target did not answer
	Handoff         string          `json:"handoff,omitempty"`         // hand-off file written when a copilot dispatch hit its quota
	Transport       *Transport      `json:"transport,omitempty"`       // stream health; fields an agent can't report stay absent
	Hops            []Hop           `json:"hops,omitempty"`            // every dispatch runWithFallback tried, in order
	Structured      json.RawMessage `json:"-"`                         // the answer as JSON satisfying RunOptions.OutputSchema
	Verify          *VerifyResult   `json:"verify,omitempty"`          // the spec's verify run, set by ask after the answer
	Handback        *Handback       `json:"handback,omitempty"`        // the typed outcome the run's handback call reported
	Recovered       []*retry.Error  `json:"-"`                         // attempts that failed before the successful one, for the hop's errors
	NotStarted      bool            `json:"-"`                         // refused before any child process or request started
	Asked           string          `json:"-"`                         // the agent the caller asked, set with Runner
	Runner          string          `json:"runner,omitempty"`          // the runner that carried Asked's run (Agent); "" when Agent ran itself
	Type            string          `json:"type,omitempty"`            // the agent type of the model that ran
}

// Hop is one dispatch runWithFallback tried; ErrorKind is the envelope error token, empty when the dispatch answered.
type Hop struct {
	Agent          string `json:"agent"`
	Account        string `json:"account,omitempty"`
	Profile        string `json:"profile,omitempty"`
	Model          string `json:"model,omitempty"`
	Reported       string `json:"reported_model,omitempty"` // Result.Reported of this hop
	RunFor         string `json:"run_for,omitempty"`        // the asked agent a pi runner hop executes for
	ErrorKind      string `json:"error_kind,omitempty"`
	Codemode       string `json:"codemode,omitempty"`        // this hop's effective code mode: off on an own CLI
	CodemodeReason string `json:"codemode_reason,omitempty"` // why a requested code mode ran off on this hop
	// NotStarted marks a dispatch refused before it started (target, permit or account): it never ran.
	NotStarted bool `json:"not_started,omitempty"`
	// Errors lists every attempt's failure, oldest first, when the dispatch was retried.
	Errors []EnvelopeError `json:"errors,omitempty"`
}

// Transport is a run's stream health: attempts, retries the agent recovered from, and how steady the stream was.
type Transport struct {
	Attempts      int    `json:"attempts"`                 // tries the outer retry loop made
	Retries       int    `json:"retries,omitempty"`        // api_retry events Claude recovered from
	RetryStatuses []int  `json:"retry_statuses,omitempty"` // their HTTP status; 0 = no response
	FirstEventMs  int64  `json:"first_event_ms,omitempty"` // start → first stream_event
	MaxGapMs      int64  `json:"max_gap_ms,omitempty"`     // longest silence between stream_events
	Proxy         string `json:"proxy,omitempty"`          // the proxy route label the dispatch ran through
}

// Result.Status values.
const (
	StatusOK      = "ok"
	StatusError   = "error"
	StatusPartial = "partial"
	StatusQuota   = "quota"
	StatusStalled = "stalled"
)
