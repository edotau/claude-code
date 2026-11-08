package agent

import (
	"errors"
	"fmt"
	"time"

	"github.com/edotau/claude-code/internal/agent/retry"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
)

// exhaustedSince is a KindQuota error when dispatch's account is marked exhausted right now, else nil.
func exhaustedSince(disp Dispatch) *retry.Error {
	if disp.Account == "" || disp.Account == copilotLoginAccount() { // copilot's own gate falls back to Databricks instead
		return nil
	}
	now := time.Now()
	e := loadState()[disp.Account]
	if e.Status(now) != router.StateExhausted {
		return nil
	}
	return &retry.Error{Kind: retry.KindQuota, Agent: disp.Agent.Name(), Err: errors.New(router.ExhaustedReason(disp.Account, e, now))}
}

// authMarkedSince is a KindAuth error when dispatch's account was marked auth-dead after the chain was built, else nil.
func authMarkedSince(disp Dispatch) *retry.Error {
	if disp.Account == "" {
		return nil
	}
	reason, marked := loadState().AuthMarked(disp.Account, time.Now())
	if !marked {
		return nil
	}
	return &retry.Error{Kind: retry.KindAuth, Agent: disp.Agent.Name(), AuthReason: reason, Err: errors.New(disp.Account + ": " + reason)}
}

// rateLimitHold is how long a plain 429 on a subscription account (no parsed reset) keeps it out of the picks.
const rateLimitHold = 10 * time.Minute

// noteQuota remembers a KindQuota failure until its reset; a subscription KindRateLimit one for 10m.
func noteQuota(disp Dispatch, err error) {
	e, ok := holdable(disp, err)
	if !ok {
		return
	}
	now := time.Now()
	until := now.Add(rateLimitHold)
	if e.Kind == retry.KindQuota {
		until = router.ResetFor(err.Error(), disp.Billing, loadState()[disp.Account], now)
	} else if t, ok := router.ParseReset(err.Error(), now); ok {
		until = t
	}
	_ = markExhausted(disp.Account, until, now, FallbackReason(err)) // best-effort: the run's own error still reports it
}

// holdable is err's *Error when it should bench the account: a quota, or a rate limit on a subscription plan.
func holdable(disp Dispatch, err error) (*retry.Error, bool) {
	var e *retry.Error
	if disp.Account == "" || !errors.As(err, &e) {
		return nil, false
	}
	switch e.Kind {
	case retry.KindQuota:
		return e, true
	case retry.KindRateLimit: // a per-endpoint 429 on a credits account must not bench the whole account
		return e, disp.Billing == router.BillSubscription
	}
	return nil, false
}

// NoteCopilotQuota marks the pinned copilot account exhausted by cause and returns that account's route-state key.
func NoteCopilotQuota(cause error) string {
	acct := copilotLoginAccount()
	noteQuota(Dispatch{Account: acct, Billing: router.BillSubscription}, retry.Classify("copilot", cause, false))
	return acct
}

// noteStall remembers a stalled dispatch for 15 minutes, so the sub-pick skips it instead of hanging again.
func noteStall(disp Dispatch, err error) {
	if disp.Account == "" || !errors.Is(err, ErrStalled) {
		return
	}
	now := time.Now()
	reason := fmt.Sprintf("dispatch stalled (no output in %s)", firstOutputDeadline())
	_ = markExhausted(disp.Account, now.Add(15*time.Minute), now, reason) // best-effort: the run's own error still reports it
}

// condemnAuth tags a KindAuth whose Recover failed with a condemning verdict, or any error that itself condemns
// (an initial u2m invalid_grant / missing refresh token can classify as fatal with no Recover run).
func condemnAuth(e *retry.Error, recoverErr error) {
	switch {
	case e.Kind == retry.KindAuth && providers.Verdict(recoverErr).Condemns():
		e.AuthReason = FallbackReason(recoverErr)
	case e.Err != nil && providers.Verdict(e.Err).Condemns():
		e.AuthReason = FallbackReason(e.Err)
	}
}

// noteAuth marks the dispatch's account auth-dead for a condemned KindAuth, so Pick skips it until a login clears it.
func noteAuth(disp Dispatch, err error) {
	var e *retry.Error
	if disp.Account == "" || !errors.As(err, &e) || e.AuthReason == "" {
		return
	}
	_ = markAuth(disp.Account, e.AuthReason) // best-effort: the run's own error still reports it
}
