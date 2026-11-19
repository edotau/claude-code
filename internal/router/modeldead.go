package router

import (
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
)

// ModelKey is the canonical model id, so marked ([1m]) and unmarked spellings of one model share a key.
func ModelKey(model string) string { return models.ModelKey(model) }

// MarkModelDead remembers account cannot use model until until; the longer hold wins and expired keys are pruned.
func MarkModelDead(account, model string, until, now time.Time) (bool, error) {
	if providers.IsFamily(model) { // a family word names no endpoint: only the concrete id that failed is recorded
		return false, nil
	}
	return tryUpdate(authMarkWait, func(s State) {
		e := s[account]
		e.ModelDead = extendHold(e.ModelDead, ModelKey(model), until, now)
		s[account] = e
	})
}

// ClearModelDead drops account's quota-dead mark on model while it still reads held; a newer or longer mark stays.
func ClearModelDead(account, model string, held time.Time) (bool, error) {
	key := ModelKey(model)
	if !sharedState()[account].ModelDead[key].Equal(held) {
		return true, nil
	}
	return tryUpdate(authMarkWait, func(s State) {
		e := s[account]
		if !e.ModelDead[key].Equal(held) { // extended while this caller waited for the lock
			return
		}
		e.ModelDead = maps.Clone(e.ModelDead)
		delete(e.ModelDead, key)
		s[account] = e
	})
}

// PerModelQuota reports an account whose 429s are per model (agy's Google route), so one holds only that model.
func PerModelQuota(account string) bool { return account == "google·antigravity" }

// ModelDead reports a live quota-dead mark on account's model.
func (s State) ModelDead(account, model string, now time.Time) bool {
	return s[account].ModelDead[ModelKey(model)].After(now)
}

// deadAccounts is what candidateDead reads for account: the account, then its profile's gateway account.
func deadAccounts(acct accountFn, account string) []string {
	out := []string{account}
	if _, p, ok := strings.Cut(account, "·"); ok && p != "" && p != "own" {
		out = append(out, acct(p))
	}
	return out
}

// modelDeadOn reports a live quota-dead mark on model under any of accounts.
func (s State) modelDeadOn(accounts []string, model string, now time.Time) bool {
	return slices.ContainsFunc(accounts, func(a string) bool { return s.ModelDead(a, model, now) })
}

// candidateDead reports a live mark on c's model (a family rung: resolved on its row, only when its family is marked).
func (s State) candidateDead(reg *providers.Registry, c Candidate, now time.Time) bool {
	accounts := deadAccounts(accountIn(reg), c.Account)
	if !providers.IsFamily(c.Model) {
		return s.modelDeadOn(accounts, c.Model, now)
	}
	family := strings.ToLower(strings.TrimSpace(c.Model))
	marked := slices.ContainsFunc(accounts, func(a string) bool {
		for key, until := range s[a].ModelDead {
			if f, _, ok := providers.FamilyVersion(key); ok && f == family && until.After(now) {
				return true
			}
		}
		return false
	})
	return marked && s.modelDeadOn(accounts, providers.ConcreteModel(c.Agent, CandidateRow(reg, c), c.Model), now)
}
