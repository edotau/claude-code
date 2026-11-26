package wthook

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/worktree"
)

const (
	fixNowIdleEnv = "HARNESS_FIXNOW_IDLE" // vault env: the owner-idle window as a Go duration ("10m")
	fixNowIdle    = 10 * time.Minute
	fixNowFresh   = 24 * time.Hour // older records are history, not work
)

// fixNowWindow is the owner-idle window: HARNESS_FIXNOW_IDLE when it parses positive, else 10 minutes.
func fixNowWindow(ctx context.Context) time.Duration {
	if d, err := time.ParseDuration(strings.TrimSpace(proc.Value(ctx, fixNowIdleEnv))); err == nil && d > 0 {
		return d
	}
	return fixNowIdle
}

// mainSession is an interactive main thread: not a subagent, a dispatched leaf or a headless (sdk-*) job.
func mainSession(ctx context.Context, in *core.Input) bool {
	return in.SessionID != "" && in.AgentID == "" && in.Agent() == core.AgentClaude && !proc.InDispatch(ctx) &&
		!strings.HasPrefix(proc.Value(ctx, "CLAUDE_CODE_ENTRYPOINT"), "sdk-")
}

// directiveDue reports whether r is a fresh record of a directive kind.
func directiveDue(r worktree.Failure) bool {
	return r.Time >= time.Now().Add(-fixNowFresh).Unix() && slices.Contains(worktree.DirectiveKinds, r.Kind)
}

// fixNowHides withholds f's plain line from a main session while the directive path owns it (sent now, or open).
func fixNowHides(ctx context.Context, main bool, sent map[string]bool, f worktree.Failure) bool {
	return sent[f.ID] || main && directiveDue(f) && worktree.FailureOpen(ctx, worktree.FailuresDir(), f)
}

// notePrompt stamps a main session's user-typed prompt; a task notification or hand-back is not the owner.
func notePrompt(ctx context.Context, in *core.Input) error {
	if !mainSession(ctx, in) || core.AutomatedDelivery(in.Prompt) {
		return nil
	}
	return worktree.NotePrompt(worktree.FailuresDir(), in.SessionID)
}

// fixNowOffer is one session's view for its directives: the idle window and whether the owner is present.
type fixNowOffer struct {
	root, session     string
	window            time.Duration
	present, fallback bool
}

// fixNowLines offers each open directive-kind failure of the checkout, newest first; fallback allows plain notices.
func fixNowLines(ctx context.Context, in *core.Input, mainRepo string, fallback bool) (lines []string, sent map[string]bool, err error) {
	if !mainSession(ctx, in) {
		return nil, nil, nil
	}
	o := fixNowOffer{root: worktree.FailuresDir(), session: in.SessionID, window: fixNowWindow(ctx), fallback: fallback}
	recs, err := worktree.ListFailures(o.root)
	if err != nil {
		return nil, nil, err
	}
	typed := in.Prompt != "" && !core.AutomatedDelivery(in.Prompt) // this very prompt is the owner, whatever the stamp says
	o.present = typed || time.Since(worktree.LastPrompt(o.root, o.session)) < o.window
	seen, sent := map[string]bool{}, map[string]bool{}
	for _, r := range slices.Backward(repoFailures(recs, mainRepo)) {
		if len(lines) >= failureLines || ctx.Err() != nil {
			break
		}
		if seen[r.ID] || !directiveDue(r) {
			continue
		}
		seen[r.ID] = true
		got, oerr := o.offer(ctx, r)
		if len(got) > 0 { // a recorded delivery is always returned
			sent[r.ID], lines = true, append(lines, got...)
		}
		if errors.Is(oerr, paths.ErrLockTimeout) || ctx.Err() != nil { // busy log or spent budget: the rest next turn
			break
		}
		err = cmp.Or(err, oerr)
	}
	return lines, sent, err
}

// offer records one delivery of r and renders it: the directive, the plain fallback notice, or nothing.
func (o fixNowOffer) offer(ctx context.Context, r worktree.Failure) ([]string, error) {
	res, err := worktree.OfferFailure(ctx, o.root, r, o.session, o.fallback)
	switch {
	case err != nil:
		return nil, err
	case res == worktree.OfferDirective:
		return []string{fixNowText(r, o.present, o.window)}, nil
	case res == worktree.OfferNotice:
		line, _ := failureNotice(r)
		return []string{line, assets.HookFailureClaim(r.ID)}, nil
	}
	return nil, nil
}

func fixNowText(r worktree.Failure, present bool, window time.Duration) string {
	name := time.Now().Format("2006-01-02") + "-autofix-" + paths.Slug(string(r.Kind), "", 20, "failure") + "-" +
		paths.Slug(r.Subject, "", 48, "failure") + ".md"
	return assets.HookFixNow(assets.FixNow{ID: r.ID, Kind: string(r.Kind), Subject: r.Subject, Summary: r.Summary,
		Report: paths.MemorySub("reports", name), Idle: window.String(), Present: present})
}

// FixNow (Stop, main session) continues the turn with each failure directive this session is due; no tool blocks.
func FixNow(in *core.Input) core.Result {
	ctx, cancel := core.HookContext("worktree-fixnow")
	defer cancel()
	_, mainRepo := paths.CheckoutOf(newRun(ctx, in).dir)
	lines, _, err := fixNowLines(ctx, in, mainRepo, false)
	res := core.Result{}
	if err != nil {
		res.Stderr = assets.HookFailuresWarn(err.Error())
	}
	if len(lines) > 0 {
		res.Decision, res.Reason = core.Block, strings.Join(lines, "\n")
	}
	return res
}
