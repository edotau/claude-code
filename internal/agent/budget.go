package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/platform/textutil"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/routing/agentconfig"
)

// handbackFiles caps the files a take-over line names.
const handbackFiles = 8

// Lane is the directory a budget-ended run worked in: its git tree, else its start directory.
func (r Record) Lane() string { return cmp.Or(r.Tree, r.Cwd) }

// stopBudget notes where the run stopped, then settles the budget fields from err.
func (r *Record) stopBudget(err error, t *tally) {
	r.stopped = &wire.StoppedAt{Event: t.lastEvent(), ToolCalls: t.toolCount(), TextBytes: t.text.Load()}
	r.settleBudget(err)
}

// settleBudget sets the budget kind from err, with the stop point and changed files a budget end carries; idempotent.
func (r *Record) settleBudget(err error) {
	r.BudgetKind = string(retry.BudgetOf(err))
	if r.BudgetKind == "" {
		return
	}
	if r.StoppedAt == nil {
		r.StoppedAt = r.stopped
	}
	if r.Changed == nil {
		r.Changed = r.changedFiles()
	}
}

// LostRanFor is how long a dead run ran: its saved duration, else start to now, since a lost run's end is unknown.
func (r Record) LostRanFor(now time.Time) time.Duration {
	if r.Status != "running" && r.DurationMs > 0 {
		return time.Duration(r.DurationMs) * time.Millisecond
	}
	return now.Sub(r.Time)
}

// LostBudgetEnd reports a dead run as a budget end: marked one, or lost past its attempt timeout; never re-sent (A4).
func (r Record) LostBudgetEnd(now time.Time) bool {
	return r.BudgetKind != "" || r.LostRanFor(now) > AttemptTimeoutFor(r.Request)
}

// NoResend reports a dead run no automatic path may re-send: cancelled by the user, or a budget end.
func (r Record) NoResend(now time.Time) bool {
	return r.cancelled() || r.LostBudgetEnd(now)
}

// SettleLostPastDeadline marks a dead run that ran past its attempt timeout a deadline budget end; true when it is one.
func (r *Record) SettleLostPastDeadline(now time.Time) bool {
	if r.BudgetKind == "" && r.LostBudgetEnd(now) {
		r.settleBudget(retry.WithBudget(errors.New(r.Error), retry.BudgetDeadline))
	}
	return r.BudgetKind != ""
}

// changedFiles is Wrote or TreeChanged, else the lane's `git status` at settle; nil when nothing is known.
func (r Record) changedFiles() []string {
	switch { // an observed empty list is an answer: the run wrote nothing, whatever else is dirty
	case r.Wrote != nil:
		return slices.Clone(*r.Wrote)
	case r.TreeChanged != nil:
		return slices.Clone(*r.TreeChanged)
	}
	lane := r.Lane()
	if lane == "" {
		return nil
	}
	snap, err := snapTree(context.Background(), lane, nil)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(snap))
	for p := range snap {
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}

// TakeOverHint is the hand-back line for a budget end: what the run left; the dispatcher sends a narrower task.
func (r Record) TakeOverHint() string {
	files := "none changed"
	if n := len(r.Changed); n > 0 {
		files = strings.Join(r.Changed[:min(n, handbackFiles)], ", ")
		if n > handbackFiles {
			files += fmt.Sprintf(" (+%d more)", n-handbackFiles)
		}
	}
	at := "unknown"
	if s := r.StoppedAt; s != nil {
		at = fmt.Sprintf("%s after %d tool calls and %d B of text", s.Event, s.ToolCalls, s.TextBytes)
	}
	return fmt.Sprintf("left lane %s, files %s, stopped at %s; dispatch a narrower task for the rest (%s budget)",
		cmp.Or(r.Lane(), "(none recorded)"), files, at, r.BudgetKind)
}

// stampBudget copies a budget end's lane, files and stop point onto env.
func (r Record) stampBudget(env *wire.Envelope) {
	if r.BudgetKind == "" {
		return
	}
	env.BudgetKind, env.StoppedAt, env.Lane, env.Changed = r.BudgetKind, r.StoppedAt, r.Lane(), r.Changed
}

// EnvelopeOf is wire.NewEnvelope with the saved record's budget end (lane, files, stop point) stamped on.
func EnvelopeOf(res wire.Result, err error, id string) wire.Envelope {
	env := wire.NewEnvelope(res, err, id)
	if env.BudgetKind != "" && id != "" {
		if rec, lerr := LoadRecord(id); lerr == nil {
			rec.stampBudget(&env)
		}
	}
	return env
}

// budgetCutoffHint wraps a budget-ended err with the take-over line.
func budgetCutoffHint(err error, rec *Record, who string, took time.Duration) error {
	return &cutoffError{err: err, hint: fmt.Sprintf("%s stopped on its %s budget after %s; %s",
		who, rec.BudgetKind, took.Round(time.Second), rec.TakeOverHint())}
}

// Cutoff values: how a run was cut off, recorded alongside fail_kind.
const (
	CutoffDeadline  = "deadline"   // the attempt deadline (--timeout or the tools default)
	CutoffStall     = "stall"      // no output before the first model output
	CutoffIdle      = "idle"       // silent after output had started
	CutoffCancelled = "cancelled"  // stopped from outside: a signal or `agents cancel`
	CutoffAuthRetry = "auth-retry" // stalled on API retries, which are not output (claude's "no model output")
)

// CutOff reports a run a limit ended (deadline, stall, silence, API retries), not a signal or a plain failure.
func (r Record) CutOff() bool {
	switch r.Cutoff {
	case CutoffDeadline, CutoffStall, CutoffIdle, CutoffAuthRetry:
		return true
	}
	return false
}

// CutoffOf names how err cut a run off, "" when it did not; deadline and idle read the typed budget kind.
func CutoffOf(err error) string {
	if err == nil {
		return ""
	}
	budget := retry.BudgetOf(err)
	switch {
	case retry.IsAdmission(err): // the run never started: a queue wait, not a deadline
		return ""
	case budget == retry.BudgetNone && authRetryStall(err):
		return CutoffAuthRetry
	case budget == retry.BudgetIdle || errors.Is(err, ErrSilent):
		return CutoffIdle
	case errors.Is(err, ErrStalled):
		return CutoffStall
	case budget == retry.BudgetDeadline, errors.Is(err, context.DeadlineExceeded):
		return CutoffDeadline
	case errors.Is(err, context.Canceled):
		return CutoffCancelled
	}
	return ""
}

// authRetryStall: stalled on API retries or a rejected login, which are not output; claude's idle.go has no sentinel.
func authRetryStall(err error) bool {
	msg := err.Error()
	return errors.Is(err, ErrStalled) && strings.Contains(msg, " API retries (") || strings.Contains(msg, "login rejected (")
}

// cutoffError wraps err with one line naming what was lost and the command that continues; deadlines and stalls only.
type cutoffError struct {
	err  error
	hint string
}

func (e *cutoffError) Error() string { return e.hint + ": " + e.err.Error() }
func (e *cutoffError) Unwrap() error { return e.err }

// withCutoffHint adds the continue line to a deadline or stall error; any other error comes back as it was.
func withCutoffHint(err error, rec *Record, res wire.Result, took time.Duration, t *tally) error {
	if rec.stopBudget(err, t); rec.BudgetKind != "" {
		return budgetCutoffHint(err, rec, cmp.Or(res.Agent, rec.Agent), took)
	}
	kind := CutoffOf(err)
	if kind == "" || kind == CutoffCancelled {
		return err
	}
	who := cmp.Or(res.Agent, rec.Agent)
	next := "continue with: claude-code agents ask --retry " + rec.ID
	if res.SessionID != "" {
		next = "continue with: claude-code agents ask --session " + res.SessionID + " --overlay " +
			textutil.ShellQuote(agentconfig.Overlay{Agent: who}.Doc()) + " 'finish from what you have'"
	}
	lost := "no answer"
	if n := t.text.Load(); n > 0 {
		lost = fmt.Sprintf("%d B of text streamed", n)
	}
	hint := fmt.Sprintf("%s cut off (%s) after %s with %d tool calls and %s; %s",
		who, kind, took.Round(time.Second), t.toolCount(), lost, next)
	if rec.RanIdentity == "" {
		rec.RanIdentity = proxy.ReadRanIdentity(rec.ID)
	}
	if rec.RanIdentity != "" {
		hint += "; ran as the " + rec.RanIdentity + " identity"
	}
	hint += "; or resume in a fresh session: claude-code agents ask --retry " + rec.ID + " --recover"
	if who == "gemini" {
		hint += "; agy log: " + AgyLogPath(rec.ID)
	}
	return &cutoffError{err: err, hint: hint}
}
