package route

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
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
	UsedPct        float64   `json:"used_pct"`
	Window         string    `json:"window,omitempty"`
	ResetsAt       time.Time `json:"resets_at,omitzero"`
	ExhaustedUntil time.Time `json:"exhausted_until,omitzero"`
	Reason         string    `json:"reason,omitempty"`
	Seen           time.Time `json:"seen"`
}

// State is every account's entry, keyed by account key.
type State map[string]Entry

// StatePath is quota.json under the harness state dir (CLAUDE_CONFIG_DIR-scoped, so tests point it at a temp dir).
func StatePath() string { return filepath.Join(paths.StateDir(), "quota.json") }

// LoadState reads quota.json; missing or corrupt reads as empty, never an error a pick must handle.
func LoadState() State {
	raw, err := os.ReadFile(StatePath())
	if err != nil {
		return State{}
	}
	var s State
	if json.Unmarshal(raw, &s) != nil || s == nil {
		return State{}
	}
	return s
}

// Update applies fn to the stored state under the file lock and writes it back (0600); best-effort.
func Update(fn func(State)) error {
	path := StatePath()
	return paths.WithFileLock(path, func() error {
		s := LoadState()
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
	quotaRE   = regexp.MustCompile(`\b402\b|payment required|exceeded your (?:monthly|current|weekly) (?:quota|limit)|usage limit|insufficient_quota|limit: 0|quota exceeded for metric|out of (?:premium )?(?:credits|requests)|monthly quota`)
	clockRE   = regexp.MustCompile(`try again (?:at|after) (\d{1,2}):(\d{2})\s*([ap]\.?m\.?)?`)
	inRE      = regexp.MustCompile(`try again in (\d+)\s*(s|sec|seconds?|m|min|minutes?|h|hours?|d|days?)\b`)
	dateRE    = regexp.MustCompile(`(?:until|resets?(?: on)?|try again (?:on|after)) (\d{4}-\d{2}-\d{2})(?:[t ](\d{2}:\d{2}(?::\d{2})?)(z|[+-]\d{2}:?\d{2})?)?`)
	monthlyRE = regexp.MustCompile(`monthly`)
)

// IsQuota reports whether an error text is an exhausted account (credits, plan window, zero quota), not a
// transient rate limit worth waiting out.
func IsQuota(text string) bool { return quotaRE.MatchString(strings.ToLower(text)) }

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
