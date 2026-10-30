package router

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/schemas"
)

// Thresholds on an account's used share of its window.
const (
	LowPct       = 90.0
	ExhaustedPct = 100.0
	StaleAfter   = 15 * time.Minute
)

// Account states a pick sorts by.
const (
	StateOK        = "ok"
	StateLow       = "low"
	StateExhausted = "exhausted"
	StateUnknown   = "unknown"
)

// Entry is one account's remembered quota.
type Entry struct {
	UsedPct        float64              `json:"used_pct"`
	Window         string               `json:"window,omitempty"`
	ResetsAt       time.Time            `json:"resets_at,omitzero"`
	ExhaustedUntil time.Time            `json:"exhausted_until,omitzero"`
	Reason         string               `json:"reason,omitempty"`
	Seen           time.Time            `json:"seen"`
	AuthReason     string               `json:"auth_reason,omitempty"`
	AuthAt         time.Time            `json:"auth_at,omitzero"`
	BYOK           map[string]time.Time `json:"byok,omitempty"`
	BYOKSkip       map[string]time.Time `json:"byok_skip,omitempty"` // "agent|entry" -> when it was marked unusable
	Unserved       map[string]time.Time `json:"unserved,omitempty"`  // wire model -> when its endpoint was found gone
}

// State is every account's entry, keyed by account key.
type State map[string]Entry

// StatePath is quota.json under the harness state dir (CLAUDE_CONFIG_DIR-scoped, so tests point it at a temp dir).
func StatePath() string { return filepath.Join(paths.StateDir(), "quota.json") }

// LoadState reads quota.json; missing, corrupt or newer-schema reads as empty, never an error a pick must handle.
func LoadState() State {
	s, _ := loadState()
	return s
}

// loadState is LoadState plus the decode error; a missing file is empty with no error.
func loadState() (State, error) {
	raw, err := os.ReadFile(StatePath())
	if err != nil {
		return State{}, nil
	}
	s, err := decodeState(raw)
	if err != nil {
		return State{}, err
	}
	return s, nil
}

// Update applies fn to the stored state under the file lock and writes it back (0600); best-effort.
// A newer-schema document is left untouched and its ErrNewer error returned.
func Update(fn func(State)) error {
	path := StatePath()
	return paths.WithFileLock(path, func() error {
		s, err := loadState()
		if errors.Is(err, schemas.ErrNewer) {
			return err
		}
		fn(s)
		data, err := json.MarshalIndent(s, "", "  ")
		if err != nil {
			return err
		}
		return paths.AtomicWrite(path, data, 0o600)
	})
}

// Record stores a fetched window reading for key; an exhaustion remembered from a failure survives a lower read
// until it expires, since a usage endpoint can lag the upstream's own verdict.
func Record(key string, usedPct float64, window string, resetsAt, now time.Time) error {
	return Update(func(s State) {
		e := s[key]
		e.UsedPct, e.Window, e.ResetsAt, e.Seen = usedPct, window, resetsAt, now
		if usedPct >= ExhaustedPct && resetsAt.After(e.ExhaustedUntil) {
			e.ExhaustedUntil, e.Reason = resetsAt, "usage at 100%"
		}
		s[key] = e
	})
}

// MarkExhausted remembers a quota failure on key until until.
func MarkExhausted(key string, until, now time.Time, reason string) error {
	return Update(func(s State) {
		e := s[key]
		if until.After(e.ExhaustedUntil) || !e.ExhaustedUntil.After(now) { // a concurrent shorter default never wins
			e.ExhaustedUntil, e.Reason = until, reason
		}
		e.Seen = now
		s[key] = e
	})
}

// GateAuth names the auth gate; AuthMarkTTL is how long a mark outlives a missed clear.
const (
	GateAuth    = "auth"
	AuthMarkTTL = 6 * time.Hour
)

// ProviderAccount is the account key of a registry profile: <provider>·<profile>, databricks when unregistered.
func ProviderAccount(profile string) string {
	prov := "databricks"
	if reg, err := providers.Load(); err == nil {
		if p := reg.Providers[profile]; p != nil && p.Provider != "" {
			prov = p.Provider
		}
	}
	return prov + "·" + profile
}

// MarkAuth remembers an auth rejection on account until a login, credential change or the TTL clears it.
func MarkAuth(account, reason string) error {
	return Update(func(s State) {
		e := s[account]
		e.AuthReason, e.AuthAt = reason, time.Now()
		s[account] = e
	})
}

// ClearAuth drops account's auth mark; nothing is written when none exists.
func ClearAuth(account string) error {
	if e, ok := LoadState()[account]; !ok || e.AuthAt.IsZero() {
		return nil
	}
	return Update(func(s State) {
		e, ok := s[account]
		if !ok {
			return
		}
		e.AuthReason, e.AuthAt = "", time.Time{}
		s[account] = e
	})
}

// ClearProviderAuth drops every auth mark on provider: the router's (ProviderAccount) and each leg's "<provider>·<p>".
func ClearProviderAuth(provider string) error {
	hit := func(account string, e Entry) bool {
		return !e.AuthAt.IsZero() && (account == ProviderAccount(provider) || strings.HasSuffix(account, "·"+provider))
	}
	marked := false
	for account, e := range LoadState() {
		marked = marked || hit(account, e)
	}
	if !marked {
		return nil // nothing is written when no mark exists
	}
	return Update(func(s State) {
		for account, e := range s {
			if hit(account, e) {
				e.AuthReason, e.AuthAt = "", time.Time{}
				s[account] = e
			}
		}
	})
}

// accountAuthMarked checks the account's own mark, then the router's mark on its profile ("<provider>·<p>" → p).
func (s State) accountAuthMarked(account string, now time.Time) (string, bool) {
	if reason, ok := s.AuthMarked(account, now); ok {
		return reason, ok
	}
	if _, p, ok := strings.Cut(account, "·"); ok && p != "" {
		return s.AuthMarked(ProviderAccount(p), now)
	}
	return "", false
}

// AuthMarked reports a live (unexpired) auth mark on account and its reason.
func (s State) AuthMarked(account string, now time.Time) (string, bool) {
	e, ok := s[account]
	if !ok || e.AuthAt.IsZero() || now.Sub(e.AuthAt) >= AuthMarkTTL {
		return "", false
	}
	return e.AuthReason, true
}

// Status classifies e at now: exhausted (100% or a live remembered failure), low (≥90%), ok, or unknown.
func (e Entry) Status(now time.Time) string {
	switch {
	case e.ExhaustedUntil.After(now):
		return StateExhausted
	case e.Seen.IsZero():
		return StateUnknown
	case e.UsedPct >= ExhaustedPct && (e.ResetsAt.IsZero() || e.ResetsAt.After(now)):
		return StateExhausted
	case e.UsedPct >= LowPct && (e.ResetsAt.IsZero() || e.ResetsAt.After(now)):
		return StateLow
	}
	return StateOK
}

// Until is when an exhausted entry frees up: the remembered failure's expiry, else the window reset.
func (e Entry) Until(now time.Time) time.Time {
	if e.ExhaustedUntil.After(now) {
		return e.ExhaustedUntil
	}
	return e.ResetsAt
}

var (
	clockRE   = regexp.MustCompile(`try again (?:at|after) (\d{1,2}):(\d{2})\s*([ap]\.?m\.?)?`)
	inRE      = regexp.MustCompile(`try again in (\d+)\s*(s|sec|seconds?|m|min|minutes?|h|hours?|d|days?)\b`)
	dateRE    = regexp.MustCompile(`(?:until|resets?(?: on)?|try again (?:on|after)) (\d{4}-\d{2}-\d{2})(?:[t ](\d{2}:\d{2}(?::\d{2})?)(z|[+-]\d{2}:?\d{2})?)?`)
	monthlyRE = regexp.MustCompile(`monthly`)
)

// ParseReset reads when an exhausted account frees up from the failure text; ok is false when the text names none.
func ParseReset(text string, now time.Time) (time.Time, bool) {
	low := strings.ToLower(text)
	if m := clockRE.FindStringSubmatch(low); m != nil {
		h, _ := strconv.Atoi(m[1])
		mi, _ := strconv.Atoi(m[2])
		if ap := strings.ReplaceAll(m[3], ".", ""); ap == "pm" && h < 12 {
			h += 12
		} else if ap == "am" && h == 12 {
			h = 0
		}
		t := time.Date(now.Year(), now.Month(), now.Day(), h, mi, 0, 0, now.Location())
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		return t, true
	}
	if m := inRE.FindStringSubmatch(low); m != nil {
		n, _ := strconv.Atoi(m[1])
		unit := map[byte]time.Duration{'s': time.Second, 'm': time.Minute, 'h': time.Hour, 'd': 24 * time.Hour}[m[2][0]]
		return now.Add(time.Duration(n) * unit), true
	}
	if m := dateRE.FindStringSubmatch(low); m != nil {
		layout, v := "2006-01-02", m[1]
		if m[2] != "" {
			layout, v = "2006-01-02T15:04", m[1]+"T"+m[2][:5]
			if len(m[2]) == 8 {
				layout, v = "2006-01-02T15:04:05", m[1]+"T"+m[2]
			}
			if m[3] != "" { // an explicit Z or offset wins over the local zone
				layout, v = layout+"Z07:00", v+strings.ToUpper(normalizeOffset(m[3]))
			}
		}
		if t, err := time.ParseInLocation(layout, v, now.Location()); err == nil && t.After(now) {
			return t, true
		}
	}
	if monthlyRE.MatchString(low) {
		return firstOfNextMonth(now), true
	}
	return time.Time{}, false
}

// ResetFor is the failure's own reset, else the account's known window: credits reset monthly, a subscription
// window at its stored reset, else in an hour (re-checked then, never forever).
func ResetFor(text, billing string, e Entry, now time.Time) time.Time {
	if t, ok := ParseReset(text, now); ok {
		return t
	}
	if e.ResetsAt.After(now) {
		return e.ResetsAt
	}
	if billing == BillCredits {
		return firstOfNextMonth(now)
	}
	return now.Add(time.Hour)
}

// normalizeOffset turns "+0500" into "+05:00" for the Z07:00 layout; "z" passes through.
func normalizeOffset(o string) string {
	if len(o) == 5 {
		return o[:3] + ":" + o[3:]
	}
	return o
}

func firstOfNextMonth(now time.Time) time.Time {
	u := now.UTC()
	return time.Date(u.Year(), u.Month()+1, 1, 0, 0, 0, 0, time.UTC)
}
