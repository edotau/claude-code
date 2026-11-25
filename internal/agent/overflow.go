package agent

import (
	"cmp"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
)

// overflowTokens reads N from "prompt is too long: N tokens > M maximum"; retry.IsContextOverflow classifies.
var overflowTokens = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`\b(\d{1,9}) tokens > \d+ maximum`) })

// overflowFit is the headroom a re-pick keeps over the refused prompt, in tenths (11 = 1.1 x).
const overflowFit = 11

// overflowPlan is one context-overflow decision: the re-pick with both windows, or why the run fails instead.
type overflowPlan struct {
	model, ran string
	from, to   int
	refusal    string
}

// overflowRefusal is err as a context overflow a re-pick answers: not a rate limit, a cancel, or text already streamed.
func overflowRefusal(err error) string {
	if err == nil || retry.Progressed(err) || rateLimitOnly(err) || !retry.MayRedispatch(err) {
		return ""
	}
	low := strings.ToLower(err.Error())
	e, typed := errors.AsType[*retry.Error](err)
	if typed && e.Kind == retry.KindContext {
		return low
	}
	if strings.Contains(low, "too many tokens") || !retry.IsContextOverflow(statusOf(e), low) { // a per-minute cap
		return ""
	}
	return low
}

// statusOf is e's HTTP status, 0 for nil.
func statusOf(e *retry.Error) int {
	if e == nil {
		return 0
	}
	return e.Status
}

// lastResortRow is account's row when last_resort (local): its overflow is terminal, no re-pick runs on that row.
func lastResortRow(account string) *providers.Provider {
	_, row, _ := strings.Cut(account, "·")
	if p := loadRegistryWarn().Row(row); p != nil && p.Options.LastResort {
		return p
	}
	return nil
}

// overflowCands is the windows larger than ran's on disp's profile that fit the refused prompt, and ran's own window.
func overflowCands(disp Dispatch, tools bool, account, ran, low string) ([]router.WindowPick, int) {
	_, row, _ := strings.Cut(account, "·")
	p := loadRegistryWarn().Row(row)
	if p == nil {
		return nil, models.Window(ran)
	}
	floor := 0
	if m := overflowTokens().FindStringSubmatch(low); m != nil {
		n, _ := strconv.Atoi(m[1])
		floor = n * overflowFit / 10
	}
	name := disp.Agent.Name()
	now := time.Now()
	entry := loadState()[account]
	served := router.ServedPool(p, slices.Clone(rotationPool(name, "", p, tools, now)), now)
	pool := slices.DeleteFunc(served, func(m string) bool {
		return router.HeldUntil(entry, name, m, now) != nil // a quota-held or skipped model stays out
	})
	_, restricted := router.PoolOverlay() // an owner allowlist admits the variant only through its base model
	allow1M := name != agentid.OpenCode && router.HeldUntil(entry, name, ran, now) == nil &&
		(!restricted || router.IsPoolModel(func() []string { return pool }, p, models.Strip1M(ran))) // opencode drops [1m]
	return router.LargerWindow(p, ran, pool, floor, allow1M), providers.Window(p, ran)
}

// planOverflow decides the re-pick of a non-auto request whose prompt overflowed ran's window; zero otherwise.
func planOverflow(disp Dispatch, lreq RunOptions, res wire.Result, ran string, err error) overflowPlan {
	low := overflowRefusal(err)
	if ran == "" || IsAuto(lreq.Model) || low == "" {
		return overflowPlan{}
	}
	account := ranAccount(disp.Account, res.Provider)
	if p := lastResortRow(account); p != nil {
		from := providers.Window(p, ran)
		return overflowPlan{ran: ran, from: from, refusal: fmt.Sprintf("%s (%d tokens) is on the last-resort row %s; no overflow re-pick runs there", ran, from, p.Name)}
	}
	cands, from := overflowCands(disp, lreq.Tools, account, ran, low)
	plan := overflowPlan{ran: ran, from: from}
	switch {
	case len(cands) == 0:
		plan.refusal = fmt.Sprintf("no model with a larger window than %s (%d tokens) is available", ran, from)
	case lreq.ModelTyped || lreq.Fallback == router.FallbackNone || lreq.Fallback == router.FallbackPin:
		plan.refusal = fmt.Sprintf("%s (%d tokens) is kept, not replaced; larger windows: %s", ran, from, nearest(cands))
	default:
		pick := cands[0]
		if !overflowTokens().MatchString(low) { // no prompt size: the largest window is the likeliest fit, first on a tie
			pick = slices.MaxFunc(cands, func(a, b router.WindowPick) int { return cmp.Compare(a.Window, b.Window) })
		}
		plan.model, plan.to = pick.Model, pick.Window
	}
	return plan
}

// nearest names up to three picks with their windows.
func nearest(cands []router.WindowPick) string {
	var out []string
	for _, c := range cands[:min(3, len(cands))] {
		out = append(out, fmt.Sprintf("%s (%d)", c.Model, c.Window))
	}
	return strings.Join(out, ", ")
}

// note is the re-pick's one line, naming the reason and both windows.
func (o overflowPlan) note() string {
	return fmt.Sprintf("context overflow: %s (%d tokens) → %s (%d tokens)", o.ran, o.from, o.model, o.to)
}

// refuse is err with the plan's refusal named; err itself when there is none.
func (o overflowPlan) refuse(err error) error {
	if o.refusal == "" {
		return err
	}
	return prefixErr(err, "context overflow, "+o.refusal)
}

// record adds the re-pick's reason and windows to the refused hop h.
func (o overflowPlan) record(h wire.Hop) wire.Hop {
	if o.model != "" {
		h.Errors = append(h.Errors, wire.EnvelopeError{Kind: h.ErrorKind, Message: o.note()})
	}
	return h
}

// prefixErr is err with prefix leading its message, keeping its retry Kind.
func prefixErr(err error, prefix string) error {
	e, ok := errors.AsType[*retry.Error](err)
	if !ok {
		return fmt.Errorf("%s: %w", prefix, err)
	}
	out := *e
	out.Err = fmt.Errorf("%s: %w", prefix, cmp.Or(e.Err, error(e)))
	return &out
}
