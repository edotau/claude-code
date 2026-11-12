package router

import (
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/models"
)

// ModelKey is the canonical model id, so marked ([1m]) and unmarked spellings of one model share a key.
func ModelKey(model string) string {
	id := models.Strip1M(model)
	if c, _, ok := models.Resolve(id); ok {
		id = c
	}
	return id
}

// MarkModelDead remembers account cannot use model until until; the longer hold wins and expired keys are pruned.
func MarkModelDead(account, model string, until, now time.Time) (bool, error) {
	return tryUpdate(authMarkWait, func(s State) {
		e := s[account]
		e.ModelDead = extendHold(e.ModelDead, ModelKey(model), until, now)
		s[account] = e
	})
}

// ModelDead reports a live quota-dead mark on account's model.
func (s State) ModelDead(account, model string, now time.Time) bool {
	return s[account].ModelDead[ModelKey(model)].After(now)
}

// accountModelDead checks the account's own mark, then the gateway's mark on its profile ("<provider>·<p>" → p).
func (s State) accountModelDead(account, model string, now time.Time) bool {
	return s.modelDeadOn(deadAccounts(account), model, now)
}

// deadAccounts is what accountModelDead reads for account: the account, then its profile's gateway account.
func deadAccounts(account string) []string {
	out := []string{account}
	if _, p, ok := strings.Cut(account, "·"); ok && p != "" && p != "own" {
		out = append(out, ProviderAccount(p))
	}
	return out
}

// modelDeadOn reports a live quota-dead mark on model under any of accounts.
func (s State) modelDeadOn(accounts []string, model string, now time.Time) bool {
	return slices.ContainsFunc(accounts, func(a string) bool { return s.ModelDead(a, model, now) })
}
