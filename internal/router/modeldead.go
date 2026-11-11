package router

import (
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
	if s.ModelDead(account, model, now) {
		return true
	}
	_, p, ok := strings.Cut(account, "·")
	return ok && p != "" && p != "own" && s.ModelDead(ProviderAccount(p), model, now)
}
