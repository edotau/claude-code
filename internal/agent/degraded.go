package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent/health"
	"github.com/edotau/claude-code/internal/agent/retry"
)

// TaskKey names one task: its text and its plan task ("<plan-slug> <id>", the Task: trailer form).
type TaskKey struct{ Text, PlanTask string }

// Health is whether dispatch is degraded for a task (R34): the reason, since when, and the task's failed attempts.
type Health struct {
	Degraded bool
	Reason   string
	Since    time.Time
	Attempts int
}

// Marks of text the harness appends to a task; the attempt key stops at the first.
const (
	retryHintMark = "\n\nAn earlier attempt (dispatch " // assets recovery.tmpl
	packMark      = "\n\nThe workspace context for this task is in "
)

// attemptKey hashes the task text before harness additions (retry hint, appended paths) with PlanTask.
func attemptKey(t TaskKey) string {
	text := preRecall(WithoutReviewLoop(t.Text))
	for _, m := range []string{retryHintMark, packMark} {
		text, _, _ = strings.Cut(text, m)
	}
	sum := sha256.Sum256([]byte(strings.TrimRight(text, " \t\r\n") + "\x00" + t.PlanTask))
	return hex.EncodeToString(sum[:])
}

// DispatchHealth reports whether dispatch is degraded for t; it never fails (an unreadable record reads as healthy).
func DispatchHealth(t TaskKey) Health {
	return Health(health.Of(attemptKey(t)))
}

func (r Record) taskKey() TaskKey { return TaskKey{Text: r.Request.Prompt, PlanTask: r.PlanTask} }

// NoteHealth counts a failed run against its task and clears the count and the dispatch-down state on a success.
func (r Record) NoteHealth(err error) {
	degradeOn(err)
	key := attemptKey(r.taskKey())
	switch {
	case r.Status == "ok":
		health.Succeed(key, r.Proxy != "")
	case r.Failed() && r.Cutoff != CutoffCancelled && !retry.IsCancelled(err):
		health.Fail(key, r.PlanTask, r.BudgetKind)
	}
}

// NoteStartFailure counts a run that failed before it started (prepare) against its task, like NoteHealth does.
func NoteStartFailure(t TaskKey, err error) {
	degradeOn(err)
	if !retry.IsCancelled(err) {
		health.Fail(attemptKey(t), t.PlanTask, string(retry.BudgetOf(err)))
	}
}

// degradedHint is the R34 notice that replaces the resend line of a failed run; "" while a resend is still allowed.
func (r Record) degradedHint() string {
	st := DispatchHealth(r.taskKey())
	switch {
	case !st.Degraded:
		return ""
	case r.BudgetKind != "":
		return "Budget end: " + r.TakeOverHint() + "; " + health.Notice(st.Reason)
	}
	return health.Notice(st.Reason)
}

// Degraded reports a run whose task or dispatch is degraded: it offers no resume or resend command.
func (r Record) Degraded() bool { return r.Failed() && r.degradedHint() != "" }

// degradedReason is why a failing run of r leaves its task degraded, "" when one more resend is still allowed.
func (r Record) degradedReason() string {
	st := DispatchHealth(r.taskKey())
	switch {
	case st.Degraded:
		return st.Reason
	case r.BudgetKind != "":
		return health.Budget(r.BudgetKind)
	case st.Attempts+1 >= health.MaxAttempts:
		return health.TwoFailed
	}
	return ""
}

// degradeOn marks dispatch itself down when err is the proxy's refusal of its own secret (A7), not an account fault.
func degradeOn(err error) {
	if e, ok := errors.AsType[*retry.Error](err); ok && e.ProxyRefused {
		health.Degrade(health.RouterSecret)
	}
}
