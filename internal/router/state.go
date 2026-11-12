// Package router is the agent+model pick: route table, candidate gates, served cache and each account's quota state.
package router

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/schemas"
)

// Thresholds on an account's used share of its window.
const (
	lowPct       = 90.0
	ExhaustedPct = 100.0
	StaleAfter   = 15 * time.Minute
)

// Billing types: a plan window already paid for, per-use credits, or a free third-party endpoint.
const (
	BillSubscription = "subscription"
	BillCredits      = "credits"
	BillFree         = "free"
)

// Account states a pick sorts by.
const (
	stateOK        = "ok"
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
	Checked        time.Time            `json:"checked,omitzero"`     // last refresh that recorded no reading; backs off like Seen
	CheckNote      string               `json:"check_note,omitempty"` // why that refresh recorded nothing
	AuthReason     string               `json:"auth_reason,omitempty"`
	AuthAt         time.Time            `json:"auth_at,omitzero"`
	Discovery      map[string]time.Time `json:"discovery,omitempty"`
	DiscoverySkip  map[string]time.Time `json:"discovery_skip,omitempty"` // "agent|entry" -> when it was marked unusable
	Unserved       map[string]time.Time `json:"unserved,omitempty"`       // wire model -> when its endpoint was found gone
	ModelDead      map[string]time.Time `json:"model_dead,omitempty"`     // canonical model -> quota-denied until
	Cooling        map[string]time.Time `json:"cooling,omitempty"`        // "·" (the account) -> rate-limit hold until
}

// State is every account's entry, keyed by account key.
type State map[string]Entry

// StatePath is quota.json under the harness state dir (CLAUDE_CONFIG_DIR-scoped, so tests point it at a temp dir).
func StatePath() string { return filepath.Join(paths.StateDir(), "quota.json") }

// stateCache holds quota.json's decoded state per file stamp; LoadState hands out clones.
var stateCache paths.StampCache[State]

// LoadState reads quota.json; missing, corrupt or newer-schema reads as empty, never an error a pick must handle.
func LoadState() State {
	s, _ := stateCache.Get(StatePath(), loadState)
	return s.clone()
}

// clone returns a deep copy of s: each entry's discovery maps are fresh.
func (s State) clone() State {
	out := make(State, len(s))
	for k, e := range s {
		e.Discovery = maps.Clone(e.Discovery)
		e.DiscoverySkip = maps.Clone(e.DiscoverySkip)
		e.Unserved = maps.Clone(e.Unserved)
		e.ModelDead = maps.Clone(e.ModelDead)
		e.Cooling = maps.Clone(e.Cooling)
		out[k] = e
	}
	return out
}

// loadState is LoadState plus the read or decode error; only a missing file is empty with no error.
func loadState() (State, error) {
	raw, err := os.ReadFile(StatePath())
	if errors.Is(err, os.ErrNotExist) {
		return State{}, nil
	}
	if err != nil {
		return State{}, err
	}
	return decodeState(raw)
}

// update applies fn under the file lock; unreadable/newer state stays untouched, corrupt state keeps one backup.
func update(fn func(State)) error {
	path := StatePath()
	return paths.WithFileLock(path, func() error { return writeState(path, fn) })
}

var authMarkWait = 2 * time.Second // bounds a request-path auth-mark write; a var so tests need not wait

// tryUpdate is update that gives up after wait; ran=false means the lock stayed busy and nothing was written.
func tryUpdate(wait time.Duration, fn func(State)) (bool, error) {
	path := StatePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	return paths.TryFileLock(path+".lock", wait, func() error { return writeState(path, fn) })
}

// writeState applies fn to the loaded state and writes it back; the caller holds the state lock.
func writeState(path string, fn func(State)) error {
	s, err := loadState()
	if err != nil {
		if errors.Is(err, schemas.ErrNewer) || !isDecodeErr(err) {
			return err
		}
		if err := backupCorruptState(path); err != nil {
			return err
		}
	}
	fn(s)
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return paths.AtomicWrite(path, data, 0o600)
}

// backupCorruptState preserves the corrupt bytes and removes earlier backups under the state lock.
func backupCorruptState(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	backup := fmt.Sprintf("%s.corrupt-%d", path, time.Now().UnixNano())
	if err := paths.AtomicWrite(backup, raw, 0o600); err != nil {
		return err
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		old := filepath.Join(filepath.Dir(path), entry.Name())
		if old != backup && entry.Type().IsRegular() && strings.HasPrefix(entry.Name(), filepath.Base(path)+".corrupt-") {
			if err := os.Remove(old); err != nil {
				return err
			}
		}
	}
	return nil
}

// isDecodeErr reports whether err came from decodeState rather than from reading the file.
func isDecodeErr(err error) bool {
	var pe *fs.PathError
	return !errors.As(err, &pe)
}

// Record stores a fetched window reading for key; a failure's remembered exhaustion survives a lower read.
func Record(key string, usedPct float64, window string, resetsAt, now time.Time) error {
	return update(func(s State) {
		e := s[key]
		e.UsedPct, e.Window, e.ResetsAt, e.Seen = usedPct, window, resetsAt, now
		e.Checked, e.CheckNote = time.Time{}, ""
		if usedPct >= ExhaustedPct && resetsAt.After(e.ExhaustedUntil) {
			e.ExhaustedUntil, e.Reason = resetsAt, "usage at 100%"
		}
		s[key] = e
	})
}

// MarkChecked stamps a refresh of key that recorded no reading, so it backs off for StaleAfter; Status is untouched.
func MarkChecked(key, note string, now time.Time) error {
	return update(func(s State) {
		e := s[key]
		e.Checked, e.CheckNote = now, note
		s[key] = e
	})
}

// Stale is true once neither a reading nor a no-data check of the account is within StaleAfter.
func (e Entry) Stale(now time.Time) bool {
	last := e.Seen
	if e.Checked.After(last) {
		last = e.Checked
	}
	return now.Sub(last) > StaleAfter
}

// MarkExhausted remembers a quota failure on key until until.
func MarkExhausted(key string, until, now time.Time, reason string) error {
	return update(func(s State) {
		e := s[key]
		if until.After(e.ExhaustedUntil) || !e.ExhaustedUntil.After(now) { // a concurrent shorter default never wins
			e.ExhaustedUntil, e.Reason = until, reason
		}
		e.Seen = now
		s[key] = e
	})
}

// GateAuth names the auth gate; authMarkTTL is how long a mark outlives a missed clear.
const (
	GateAuth    = "auth"
	authMarkTTL = 6 * time.Hour
)

// ProviderAccount is the account key of a registry profile: <provider>·<profile>, databricks when unregistered.
func ProviderAccount(profile string) string {
	if reg, err := providers.Load(); err == nil && reg.Providers[profile] != nil {
		return AccountOf(reg.Providers[profile])
	}
	return "databricks·" + profile
}

// AccountOf is ProviderAccount for a profile already in hand: no registry load.
func AccountOf(p *providers.Provider) string {
	return cmp.Or(p.Provider, "databricks") + "·" + p.Name
}

// extendHold is cur without its expired keys, with key held until until when that is later than its current hold.
func extendHold(cur map[string]time.Time, key string, until, now time.Time) map[string]time.Time {
	out := map[string]time.Time{}
	for k, t := range cur {
		if t.After(now) {
			out[k] = t
		}
	}
	if until.After(now) && until.After(out[key]) {
		out[key] = until
	}
	return out
}

// MarkAuth remembers an auth rejection on account until a login, credential change or the TTL clears it.
func MarkAuth(account, reason string) error {
	return update(markAuth(account, reason))
}

// TryMarkAuth is MarkAuth bounded by authMarkWait; ran=false means the lock stayed busy and no mark was written.
func TryMarkAuth(account, reason string) (bool, error) {
	return tryUpdate(authMarkWait, markAuth(account, reason))
}

func markAuth(account, reason string) func(State) {
	return func(s State) {
		e := s[account]
		e.AuthReason, e.AuthAt = reason, time.Now()
		s[account] = e
	}
}

// ClearAuth drops account's auth mark within authMarkWait; nothing is written when none exists.
func ClearAuth(account string) error {
	if e, ok := LoadState()[account]; !ok || e.AuthAt.IsZero() {
		return nil
	}
	ran, err := tryUpdate(authMarkWait, clearAuth(account))
	if err == nil && !ran {
		return fmt.Errorf("%w: %s.lock", paths.ErrLockTimeout, StatePath())
	}
	return err
}

func clearAuth(account string) func(State) {
	return func(s State) {
		e, ok := s[account]
		if !ok {
			return
		}
		e.AuthReason, e.AuthAt = "", time.Time{}
		s[account] = e
	}
}

// ClearProviderAuth drops provider's auth and model-dead marks: the router's own and each dispatch's "<provider>·<p>".
func ClearProviderAuth(provider string) error {
	hit := func(account string, e Entry) bool {
		return (!e.AuthAt.IsZero() || len(e.ModelDead) > 0) && strings.HasSuffix(account, "·"+provider)
	}
	marked := false
	for account, e := range LoadState() {
		marked = marked || hit(account, e)
	}
	if !marked {
		return nil // nothing is written when no mark exists
	}
	return update(func(s State) {
		for account, e := range s {
			if hit(account, e) {
				e.AuthReason, e.AuthAt, e.ModelDead = "", time.Time{}, nil
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
	if !ok || e.AuthAt.IsZero() || now.Sub(e.AuthAt) >= authMarkTTL {
		return "", false
	}
	return e.AuthReason, true
}

// Status classifies quota; stale exhausted readings without a reset are unknown, live failure marks remain exhausted.
func (e Entry) Status(now time.Time) string {
	switch {
	case e.ExhaustedUntil.After(now):
		return StateExhausted
	case e.Seen.IsZero():
		return StateUnknown
	case e.ResetsAt.IsZero():
		if e.UsedPct >= ExhaustedPct && now.Sub(e.Seen) > StaleAfter {
			return StateUnknown
		}
	case !e.ResetsAt.After(now):
		return stateOK
	}
	switch {
	case e.UsedPct >= ExhaustedPct:
		return StateExhausted
	case e.UsedPct >= lowPct:
		return StateLow
	}
	return stateOK
}

// Until is when an exhausted entry frees up: the remembered failure's expiry, else the window reset.
func (e Entry) Until(now time.Time) time.Time {
	if e.ExhaustedUntil.After(now) {
		return e.ExhaustedUntil
	}
	return e.ResetsAt
}

var (
	clockRE = regexp.MustCompile(`try again (?:at|after) (\d{1,2}):(\d{2})\s*([ap]\.?m\.?)?`)
	inRE    = regexp.MustCompile(`try again in (\d+)\s*(s|sec|seconds?|m|min|minutes?|h|hours?|d|days?)\b`)
	dateRE  = regexp.MustCompile(`(?:until|resets?(?: on)?|try again (?:on|after)) (\d{4}-\d{2}-\d{2})(?:[t ](\d{2}:\d{2}(?::\d{2})?)(z|[+-]\d{2}:?\d{2})?)?`)
)

// ParseReset reads when an exhausted account frees up from the failure text; ok is false when the text names none.
func ParseReset(text string, now time.Time) (time.Time, bool) {
	low := strings.ToLower(text)
	for _, parse := range []func(string, time.Time) (time.Time, bool){parseClockReset, parseInReset, parseDateReset} {
		if t, ok := parse(low, now); ok {
			return t, true
		}
	}
	if strings.Contains(low, "monthly") {
		return firstOfNextMonth(now), true
	}
	return time.Time{}, false
}

// parseClockReset reads "try again at|after H:MM [am|pm]" as the next such wall-clock time.
func parseClockReset(low string, now time.Time) (time.Time, bool) {
	m := clockRE.FindStringSubmatch(low)
	if m == nil {
		return time.Time{}, false
	}
	h, _ := strconv.Atoi(m[1])
	mi, _ := strconv.Atoi(m[2])
	if !validClock(h, mi, m[3]) {
		return time.Time{}, false
	}
	if ap := strings.ReplaceAll(m[3], ".", ""); ap == "pm" && h < 12 {
		h += 12
	} else if ap == "am" && h == 12 {
		h = 0
	}
	t := time.Date(now.Year(), now.Month(), now.Day(), h, mi, 0, 0, now.Location())
	if !t.After(now) {
		t = time.Date(now.Year(), now.Month(), now.Day()+1, h, mi, 0, 0, now.Location())
	}
	return t, true
}

// validClock reports whether h:mi is a real time; a 12-hour clock (ampm set) needs h in 1..12.
func validClock(h, mi int, ampm string) bool {
	return mi <= 59 && h <= 23 && (ampm == "" || (h >= 1 && h <= 12))
}

// parseInReset reads "try again in N unit" as now plus that duration.
func parseInReset(low string, now time.Time) (time.Time, bool) {
	m := inRE.FindStringSubmatch(low)
	if m == nil {
		return time.Time{}, false
	}
	unit := map[byte]time.Duration{'s': time.Second, 'm': time.Minute, 'h': time.Hour, 'd': 24 * time.Hour}[m[2][0]]
	const maxIn = 365 * 24 * time.Hour
	n, err := strconv.Atoi(m[1])
	if err != nil || n < 0 || time.Duration(n) > maxIn/unit {
		return now.Add(maxIn), true // clamp: a huge N must not wrap into the past
	}
	return now.Add(time.Duration(n) * unit), true
}

// parseDateReset reads an ISO date, with optional time and zone, that lies in the future.
func parseDateReset(low string, now time.Time) (time.Time, bool) {
	m := dateRE.FindStringSubmatch(low)
	if m == nil {
		return time.Time{}, false
	}
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
	return time.Time{}, false
}

// normalizeOffset turns "+0500" into "+05:00" for the Z07:00 layout; "z" passes through.
func normalizeOffset(o string) string {
	if len(o) == 5 {
		return o[:3] + ":" + o[3:]
	}
	return o
}

// ResetFor is the failure's own reset, else the stored window reset; credits reset monthly, else in an hour.
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

func firstOfNextMonth(now time.Time) time.Time {
	u := now.UTC()
	return time.Date(u.Year(), u.Month()+1, 1, 0, 0, 0, 0, time.UTC)
}

// quota.json stays the bare account map on disk (v0): a still-running old daemon only reads that shape.
const schemaVersion = 0

// stateFile is the {schema,accounts} wrapper a pre-revert build wrote; readers still accept it.
type stateFile struct {
	Schema   int   `json:"schema"`
	Accounts State `json:"accounts"`
}

var _ = schemas.Register(schemas.Format{
	Name: "quota", Owner: "internal/router", Kind: schemas.Derived,
	Glob:    StatePath,
	Version: schemaVersion, Keep: "bare map, no schema field: a running old daemon reads it; marks expire on their own",
})

// decodeState reads either shape; a newer-schema wrapper returns an ErrNewer error and an empty state.
func decodeState(raw []byte) (State, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil || probe == nil {
		return State{}, errors.New("quota: not a JSON object")
	}
	_, hasSchema := probe["schema"]
	_, hasAccounts := probe["accounts"]
	if hasSchema && hasAccounts {
		var f stateFile
		if err := json.Unmarshal(raw, &f); err != nil {
			return State{}, err
		}
		if f.Schema > 1 {
			return State{}, fmt.Errorf("quota v%d > v1: %w", f.Schema, schemas.ErrNewer)
		}
		if f.Accounts == nil {
			f.Accounts = State{}
		}
		return f.Accounts, nil
	}
	var s State
	if err := json.NewDecoder(bytes.NewReader(raw)).Decode(&s); err != nil {
		return State{}, err
	}
	if s == nil {
		s = State{}
	}
	return s, nil
}

// ClearSecretAuth drops the auth marks of every provider whose sealed secret or auth.env is name; best-effort.
func ClearSecretAuth(name string) {
	reg, err := providers.Load()
	if err != nil {
		return
	}
	for _, p := range reg.Providers {
		if providers.AuthSecretName(p) == name || (p.Auth.Env != "" && p.Auth.Env == name) {
			_ = ClearProviderAuth(p.Name)
		}
	}
}

// CurrentLimits reads the registry and router state; any error fails open to Base.
func CurrentLimits() inflight.Limits {
	cpus := runtime.NumCPU()
	reg, err := providers.Load()
	if err != nil {
		return inflight.ComputeLimits(nil, nil, cpus)
	}
	state, now := LoadState(), time.Now()
	var available []string
	override := map[string]int{}
	for _, p := range reg.Providers {
		account := AccountOf(p)
		if p.Concurrency > 0 {
			override[account] = p.Concurrency
		}
		if !providers.CredentialPresent(p) || state[account].Status(now) == StateExhausted {
			continue
		}
		if _, marked := state.AuthMarked(account, now); marked {
			continue
		}
		available = append(available, account)
	}
	slices.Sort(available)
	return inflight.ComputeLimits(available, override, cpus)
}

// SessionAccount is the account this session bills: the loopback /p/<name>/ profile, else pin, else the default.
func SessionAccount() string {
	name := providers.SessionLead(nil, true)
	if u, err := url.Parse(os.Getenv("ANTHROPIC_BASE_URL")); err == nil && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost") {
		if rest, ok := strings.CutPrefix(u.Path, "/p/"); ok {
			name, _, _ = strings.Cut(rest, "/")
		}
	}
	if name == "" {
		if reg, err := providers.Load(); err == nil {
			name = reg.Default
		}
	}
	return ProviderAccount(name)
}
