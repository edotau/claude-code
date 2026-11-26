package worktree

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// maxRevalRounds bounds how often one land re-integrates onto a moved target before it fails as before.
const maxRevalRounds = 3

// errTargetMoved marks a target that moved since the integration; the lane-land hook retries on its text.
var errTargetMoved = errors.New("target moved")

// attempt times one land attempt and writes its LandAttempt record.
type attempt struct {
	a LandAttempt
	t time.Time
}

func startAttempt(repo *Repo, req LandRequest) *attempt {
	now := time.Now()
	return &attempt{t: now, a: LandAttempt{Time: now.Unix(), Repo: repo.Main, Lane: req.Name, Attempt: 1,
		By: cmp.Or(os.Getenv(LandWaitEnv), "manual"), Scope: ScopeNone, Reval: RevalNone,
		Seconds: StageSeconds{Wait: req.waited.Seconds()}}}
}

// stage adds the time since the last mark to dst.
func (r *attempt) stage(dst *float64) {
	now := time.Now()
	*dst += now.Sub(r.t).Seconds()
	r.t = now
}

// pin records the plan's pinned OIDs and the stage that just ended.
func (r *attempt) pin(plan *LandPlan, dst *float64) {
	r.a.TargetOID, r.a.IntegratedOID = plan.TargetOID, plan.IntegratedOID
	r.stage(dst)
}

// finish records the open attempt with the outcome its error maps to.
func (r *attempt) finish(err error) { r.next(outcomeOf(err)) }

// next records the open attempt as o and opens the following one, whose Reason is o.
func (r *attempt) next(o Outcome) {
	r.a.Outcome = o
	RecordAttempt(r.a)
	r.a = LandAttempt{Time: time.Now().Unix(), Repo: r.a.Repo, Lane: r.a.Lane, Attempt: r.a.Attempt + 1, By: r.a.By,
		Scope: r.a.Scope, Packages: r.a.Packages, Reval: RevalNone, Reason: o}
	r.t = time.Now()
}

func outcomeOf(err error) Outcome {
	switch {
	case err == nil:
		return OutcomeLanded
	case errors.Is(err, errLandBusy):
		return OutcomeLockBusy
	case errors.Is(err, ErrRebaseConflict):
		return OutcomeConflict
	case errors.Is(err, errTargetMoved):
		return OutcomeTargetMoved
	case errors.Is(err, ErrLandRefused) && strings.Contains(err.Error(), issueDirtyTarget):
		return OutcomeDirtyTarget
	case errors.Is(err, ErrLandRefused):
		return OutcomeRefused
	case strings.Contains(err.Error(), "verify failed"):
		return OutcomeGateFailed
	}
	return OutcomeError
}

func scopeOf(gate, source string, req LandRequest) Scope {
	switch {
	case gate == "":
		return ScopeNone
	case req.Verify != "":
		return ScopeCustom
	case strings.HasPrefix(source, scopedGateSource):
		return ScopeScoped
	}
	return ScopeFull
}

// gated adds a gate's seconds per step to the attempt: build and vet apart, every other step as test time.
func (r *attempt) gated(secs map[string]float64) {
	for name, s := range secs {
		switch name {
		case "build":
			r.a.Seconds.Build += s
		case "vet":
			r.a.Seconds.Vet += s
		default:
			r.a.Seconds.Test += s
		}
	}
	r.t = time.Now()
}

// scopeAt lists packages in dir, the lane's NUL-joined files and its gate scope (not Scoped for the whole suite).
func scopeAt(ctx context.Context, plan *LandPlan, dir string) (list []goPkg, abs, files string, sc gateScope, err error) {
	names, err := laneFiles(ctx, plan.WorktreePath, plan.TargetOID, plan.IntegratedOID)
	if err != nil {
		return nil, "", "", sc, err
	}
	if list, err = listPackages(ctx, dir); err != nil {
		return nil, "", "", sc, err
	}
	if abs, err = filepath.Abs(dir); err == nil {
		abs, err = filepath.EvalSymlinks(abs)
	}
	if err != nil {
		return nil, "", "", sc, err
	}
	files = strings.Join(names, "\x00")
	if changed, full := changedPackages(list, abs, files); !full {
		sc = gateScope{Changed: changed, Tested: reverseDeps(list, abs, changed), Scoped: true}
	}
	return list, abs, files, sc, nil
}

// promoteOrRebase, in one land-lock hold, re-integrates onto a moved target (from = old target) or promotes.
func promoteOrRebase(ctx context.Context, repo *Repo, plan *LandPlan, note func(time.Duration, string, string),
	rebase bool) (landed *LandResult, from string, err error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	err = acquire(0, note, func() error {
		// a switched branch is left to promote's own refusal
		if branch, err := requireBranch(ctx, repo.Main); rebase && err == nil && branch == plan.TargetRef {
			head, err := headOfContext(ctx, repo.Main)
			if err != nil {
				return err
			}
			if head != plan.TargetOID {
				old := plan.TargetOID
				plan.TargetOID = head
				if landed, err = integrateTarget(ctx, plan); err != nil {
					plan.TargetOID = old
					return fmt.Errorf("integrate: %w", err)
				}
				from = old
				return nil
			}
		}
		var err error
		landed, err = promoteLocked(ctx, repo, plan)
		return err
	})
	return landed, from, err
}

// relander is one land's state across its re-check rounds.
type relander struct {
	repo         *Repo
	plan         *LandPlan
	req          LandRequest
	gate, source string
	first        gateScope
	rec          *attempt
	h            *HeldGate
}

// recheck re-gates the rebased lane outside the lock: build and vet always, tests only where the moved commits reach.
func (l *relander) recheck(ctx context.Context, from string) error {
	if err := l.h.Reset(ctx, l.plan.IntegratedOID); err != nil {
		fmt.Fprintf(l.req.Err, "re-check: gate slot reset failed (%v); gating in the lane\n", err)
		l.h = nil
	}
	dir := l.h.Where(l.plan.WorktreePath)
	list, abs, files, next, err := scopeAt(ctx, l.plan, dir)
	if err != nil {
		return err
	}
	moved, err := laneFiles(ctx, l.repo.Main, from, l.plan.TargetOID)
	if err != nil {
		return err
	}
	rv := revalidate(list, abs, l.first, files, strings.Join(moved, "\x00"))
	gate, source := l.gate, l.source
	l.rec.a.Reval, l.rec.a.Retested = RevalReused, len(rv.Retest)
	switch {
	case rv.Kind == revalFull && l.first.Scoped:
		l.rec.a.Reval, gate, source = RevalFull, fullGoGate, scopedGateSource+" (full re-gate)"
	case rv.Kind == revalFull:
		l.rec.a.Reval = RevalFull
	default:
		if rv.Kind == revalRetest {
			l.rec.a.Reval = RevalPartial
		}
		gate, source = scopedGateCommand(rv.Retest), scopedGateSource
	}
	fmt.Fprintf(l.req.Out, "re-check:   target moved to %s; reval %s\n", ShortOID(l.plan.TargetOID), l.rec.a.Reval)
	l.first = next
	return runGate(ctx, l.req, l.h.At(l.plan), gate, source, l.rec)
}

// round promotes the lane, or re-integrates and re-checks it when the target moved (again), all under one lock hold.
func (l *relander) round(ctx context.Context, rebase bool) (landed *LandResult, again bool, err error) {
	started := time.Now()
	note := func(waited time.Duration, pid, by string) {
		fmt.Fprintf(l.req.Out, "land %s: waiting for the land lock (%s; held by pid %s, %s)\n", l.req.Name,
			waited.Round(time.Second), pid, by)
	}
	landed, from, err := promoteOrRebase(ctx, l.repo, l.plan, note, rebase)
	switch {
	case from != "":
		l.rec.next(OutcomeTargetMoved)
		l.rec.t = started
		l.rec.pin(l.plan, &l.rec.a.Seconds.Rebase)
		printRebase(l.req.Out, l.plan, landed)
		if err := l.recheck(ctx, from); err != nil {
			return nil, false, err
		}
		return nil, true, nil
	case errors.Is(err, errTargetMoved) && rebase:
		l.rec.next(OutcomeTargetMoved)
		return nil, true, nil
	case errors.Is(err, ErrRebaseConflict):
		if l.req.ResolveAgent == "" {
			notifyConflict(l.plan, l.req.Err)
		}
		return nil, false, err
	}
	l.rec.stage(&l.rec.a.Seconds.Promote)
	return landed, false, err
}

// promoteLoop promotes the verified lane; a moved target re-integrates and re-checks, up to maxRevalRounds times.
func (l *relander) promoteLoop(ctx context.Context) (*LandResult, error) {
	var landed *LandResult
	var err error
	for i := 0; i <= maxRevalRounds; i++ {
		var again bool
		if landed, again, err = l.round(ctx, i < maxRevalRounds); !again {
			break
		}
	}
	if err == nil {
		return landed, nil
	}
	if landed != nil && landed.ResumeHint != "" {
		fmt.Fprintln(l.req.Err, "  retry:", landed.ResumeHint)
	}
	return nil, fmt.Errorf("promote: %w", err)
}
