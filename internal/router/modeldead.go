package router

import (
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
		dead := map[string]time.Time{}
		for k, t := range e.ModelDead {
			if t.After(now) {
				dead[k] = t
			}
		}
		if key := ModelKey(model); until.After(now) && until.After(dead[key]) {
			dead[key] = until
		}
		e.ModelDead = dead
		s[account] = e
	})
}

// ModelDead reports a live quota-dead mark on account's model.
func (s State) ModelDead(account, model string, now time.Time) bool {
	return s[account].ModelDead[ModelKey(model)].After(now)
}
