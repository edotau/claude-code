package gemini

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/textutil"
)

const (
	quotaTTL     = 60 * time.Second
	quotaTimeout = 20 * time.Second
	// LowQuotaPct is the remaining percent under which a pool or model limit draws a warning.
	LowQuotaPct   = 15.0
	googleAccount = "google·antigravity"
	// agy names its pools in /usage output; a model belongs to the pool its id prefix says.
	geminiPool = "Gemini Models"
	otherPool  = "Claude and GPT models"
)

// QuotaRow is one `agy -p "/usage"` line: a pool's or a single model's limit, percent remaining, and its reset.
type QuotaRow struct {
	Pool      string    `json:"pool"`
	Limit     string    `json:"limit"`
	Remaining float64   `json:"remaining_percent"`
	Reset     time.Time `json:"reset,omitzero"`
}

// quotaRun runs the harness's agy for /usage (spends no quota); a var so tests never reach the binary.
var quotaRun = func(ctx context.Context) ([]byte, error) {
	bin, err := paths.AgentBin("agy")
	if err != nil {
		return nil, err
	}
	return exec.CommandContext(ctx, bin, "-p", "/usage", "--print-timeout", "20s").Output()
}

var (
	quotaNow   = time.Now
	quotaMu    sync.Mutex
	quotaAt    time.Time
	quotaCache []QuotaRow
	quotaErr   error
)

// Quotas reads agy's /usage once per minute (failures included, so a dead binary costs one timeout, not one per call).
func Quotas(ctx context.Context) ([]QuotaRow, error) {
	quotaMu.Lock()
	defer quotaMu.Unlock()
	if now := quotaNow(); !quotaAt.IsZero() && now.Sub(quotaAt) < quotaTTL {
		return quotaCache, quotaErr
	}
	ctx, cancel := context.WithTimeout(ctx, quotaTimeout)
	defer cancel()
	out, err := quotaRun(ctx)
	rows := ParseQuotas(out)
	switch {
	case len(rows) > 0:
		err = nil
	case err != nil:
		err = fmt.Errorf("agy usage unavailable: %s", textutil.FirstLine(err.Error()))
	default:
		err = errors.New("agy usage unavailable: " + textutil.TruncateRunes(textutil.FirstLine(strings.TrimSpace(string(out))), 60))
	}
	quotaAt, quotaCache, quotaErr = quotaNow(), rows, err
	return rows, err
}

// ParseQuotas reads the tab-separated /usage lines; any line without a "NN%" third field is skipped.
func ParseQuotas(out []byte) []QuotaRow {
	var rows []QuotaRow
	for line := range strings.SplitSeq(string(out), "\n") {
		f := strings.Split(strings.TrimRight(line, "\r"), "\t")
		if len(f) < 3 {
			continue
		}
		pct, ok := strings.CutSuffix(strings.TrimSpace(f[2]), "%")
		v, err := strconv.ParseFloat(pct, 64)
		if !ok || err != nil || !(v >= 0 && v <= 100) {
			continue
		}
		r := QuotaRow{Pool: strings.TrimSpace(f[0]), Limit: strings.TrimSpace(f[1]), Remaining: v}
		if len(f) > 3 {
			r.Reset, _ = time.Parse(time.RFC3339, strings.TrimSpace(f[3]))
		}
		rows = append(rows, r)
	}
	return rows
}

// perModel is a limit named for one model ("Gemini 4 Argon Weekly Limit" under pool "Gemini 4 Argon").
func (r QuotaRow) perModel() bool { return r.Pool != "" && strings.HasPrefix(r.Limit, r.Pool+" ") }

// Model is the one catalog model a per-model limit names; false for a pool limit or an unknown model.
func (r QuotaRow) Model() (string, bool) {
	if !r.perModel() {
		return "", false
	}
	id, _, ok := models.Resolve(strings.ReplaceAll(strings.ToLower(r.Pool), " ", "-"))
	return id, ok
}

// Line is the row as one terminal line of at most 80 characters, the reset in local time.
func (r QuotaRow) Line() string {
	limit := strings.TrimSuffix(strings.TrimPrefix(r.Limit, r.Pool+" "), " Remaining")
	reset := "no reset time"
	if !r.Reset.IsZero() {
		reset = "resets " + r.Reset.Local().Format("2006-01-02 15:04")
	}
	return textutil.TruncateRunes(fmt.Sprintf("%-39s %3.0f%% left  %s", r.Pool+" · "+limit, r.Remaining, reset), 80)
}

// quotaKey is model's catalog id without an effort suffix, wire prefix or [1m].
func quotaKey(model string) string {
	if base, _, ok := splitEffort(model); ok {
		model = base
	}
	return models.ModelKey(model)
}

func poolOf(key string) string {
	switch {
	case strings.HasPrefix(key, "gemini-"):
		return geminiPool
	case strings.HasPrefix(key, "claude-"), strings.HasPrefix(key, "gpt-"):
		return otherPool
	}
	return ""
}

// rowsFor are the limits model draws on: its own per-model limits, else its pool's.
func rowsFor(rows []QuotaRow, model string) []QuotaRow {
	key := quotaKey(model)
	var own, pool []QuotaRow
	for _, r := range rows {
		if m, ok := r.Model(); ok {
			if m == key {
				own = append(own, r)
			}
		} else if !r.perModel() && r.Pool == poolOf(key) {
			pool = append(pool, r)
		}
	}
	if len(own) > 0 {
		return own
	}
	return pool
}

// ModelQuota is what the limits say about one model: its pool, the lowest percent left, a hold, the closest warning.
type ModelQuota struct {
	Pool      string
	Left      float64
	HeldUntil time.Time
	Low       *QuotaRow
	HoldErr   error // the mark could not be written; the status itself stands
}

func statusFor(rows []QuotaRow, model string, now time.Time) ModelQuota {
	limits := rowsFor(rows, model)
	if len(limits) == 0 {
		return ModelQuota{}
	}
	q := ModelQuota{Pool: limits[0].Pool, Left: 100}
	for _, r := range limits {
		q.Left = min(q.Left, r.Remaining)
		switch {
		case r.Remaining == 0 && r.Reset.After(now):
			if r.Reset.After(q.HeldUntil) {
				q.HeldUntil = r.Reset
			}
		case r.Remaining > 0 && r.Remaining < LowQuotaPct && (q.Low == nil || r.Remaining < q.Low.Remaining):
			q.Low = &r
		}
	}
	return q
}

// Note is the resolve note: the pool's remaining percent and, on a 0% limit, when the hold ends.
func (q ModelQuota) Note() string {
	if q.Pool == "" {
		return ""
	}
	s := fmt.Sprintf("%s %.0f%% left", q.Pool, q.Left)
	if !q.HeldUntil.IsZero() {
		s += "; held until " + q.HeldUntil.Local().Format("2006-01-02 15:04")
	}
	return s
}

// Warning is one line when a 5-hour or weekly limit of the model is under LowQuotaPct, else "".
func (q ModelQuota) Warning() string {
	if q.Low == nil {
		return ""
	}
	return "warning: google quota low: " + q.Low.Line()
}

// Check holds each 0% limit's models (model's pool, each per-model limit's own) and returns model's status.
func Check(ctx context.Context, model string, now time.Time) (ModelQuota, error) {
	rows, err := Quotas(ctx)
	if err != nil {
		return ModelQuota{}, err
	}
	q := statusFor(rows, model, now)
	q.HoldErr = holdZeroLimits(rows, model, now)
	return q, nil
}

// holdZeroLimits marks quota-dead each model whose limits are at 0% with a reset time ahead.
func holdZeroLimits(rows []QuotaRow, model string, now time.Time) error {
	keys := map[string]bool{quotaKey(model): true}
	for _, r := range rows {
		if m, ok := r.Model(); ok {
			keys[m] = true
		}
	}
	var errs []error
	state := router.LoadState()
	for key := range keys {
		until := statusFor(rows, key, now).HeldUntil
		if until.IsZero() || state[googleAccount].ModelDead[key].Compare(until) >= 0 {
			continue
		}
		ran, err := router.MarkModelDead(googleAccount, key, until, now)
		if err == nil && !ran {
			err = errors.New("route state lock busy")
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("hold %s: %w", key, err))
		}
	}
	return errors.Join(errs...)
}
