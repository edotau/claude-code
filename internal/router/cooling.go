package router

import (
	"strings"
	"time"
)

// coolingSelf is the Cooling key for the account as a whole; per-model holds can sit beside it later.
const coolingSelf = "·"

// coolingPenalty is what a cooling account loses from its 0–1 Total: it sinks among near-ties, never leaves the pick.
const coolingPenalty = 0.25

// MarkCooling remembers a transient 429 hold on account; the longer hold wins, expired keys go, ·own is never written.
func MarkCooling(account string, until, now time.Time) (bool, error) {
	if strings.HasSuffix(account, "·own") {
		return false, nil
	}
	return tryUpdate(authMarkWait, func(s State) {
		e := s[account]
		cool := map[string]time.Time{}
		for k, t := range e.Cooling {
			if t.After(now) {
				cool[k] = t
			}
		}
		if until.After(now) && until.After(cool[coolingSelf]) {
			cool[coolingSelf] = until
		}
		e.Cooling = cool
		s[account] = e
	})
}

// CoolingUntil is when account's hold ends; zero when it has none.
func (s State) CoolingUntil(account string) time.Time {
	return s[account].Cooling[coolingSelf]
}

// accountCooling checks the account's own hold, then the gateway's hold on its profile ("<provider>·<p>" → p).
func (s State) accountCooling(account string, now time.Time) bool {
	if s.CoolingUntil(account).After(now) {
		return true
	}
	_, p, ok := strings.Cut(account, "·")
	return ok && p != "" && p != "own" && s.CoolingUntil(ProviderAccount(p)).After(now)
}
