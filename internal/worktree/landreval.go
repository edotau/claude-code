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

// scopeAt lists packages, the lane's NUL-joined files and its gate scope (not Scoped when the whole suite is due).
func scopeAt(ctx context.Context, plan *LandPlan) (list []goPkg, abs, files string, sc gateScope, err error) {
	names, err := laneFiles(ctx, plan.WorktreePath, plan.TargetOID, plan.IntegratedOID)
	if err != nil {
		return nil, "", "", sc, err
	}
	if list, err = listPackages(ctx, plan.WorktreePath); err != nil {
		return nil, "", "", sc, err
	}
	if abs, err = filepath.Abs(plan.WorktreePath); err == nil {
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

// firstScope is what the first gate covered; anything but the scoped Go gate (or a failed listing) is not Scoped.
func firstScope(ctx context.Context, plan *LandPlan, source string) gateScope {
	if !strings.HasPrefix(source, scopedGateSource) {
		return gateScope{}
	}
	_, _, _, sc, err := scopeAt(ctx, plan)
	if err != nil {
		return gateScope{}
	}
	return sc
}

// rebaseIfMoved re-integrates the lane onto a moved target under the land lock; from is the old target.
func rebaseIfMoved(ctx context.Context, repo *Repo, plan *LandPlan) (res *LandResult, from string, err error) {
	err = acquire(0, func() error {
		// a switched branch is left to promote's own refusal
		branch, err := requireBranch(ctx, repo.Main)
		if err != nil || branch != plan.TargetRef {
			return nil
		}
		head, err := headOfContext(ctx, repo.Main)
		if err != nil || head == plan.TargetOID {
			return err
		}
		old := plan.TargetOID
		plan.TargetOID = head
		if res, err = integrateTarget(ctx, plan); err != nil {
			plan.TargetOID = old
			return err
		}
		from = old
		return nil
	})
	return res, from, err
}

// relander is one land's state across its re-check rounds.
type relander struct {
	repo         *Repo
	plan         *LandPlan
	req          LandRequest
	gate, source string
	first        gateScope
	rec          *attempt
}

// recheck re-gates the rebased lane outside the lock: build and vet always, tests only where the moved commits reach.
func (l *relander) recheck(ctx context.Context, from string) error {
	list, abs, files, next, err := scopeAt(ctx, l.plan)
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
	err = runGate(ctx, l.plan, l.req, gate, source)
	l.rec.stage(&l.rec.a.Seconds.Test)
	return err
}

// round re-integrates onto a moved target and re-checks it, then promotes; again reports a target that moved meanwhile.
func (l *relander) round(ctx context.Context) (landed *LandResult, again bool, err error) {
	started := time.Now()
	res, from, err := rebaseIfMoved(ctx, l.repo, l.plan)
	if err != nil {
		if errors.Is(err, ErrRebaseConflict) && l.req.ResolveAgent == "" {
			notifyConflict(l.plan, l.req.Err)
		}
		return nil, false, fmt.Errorf("integrate: %w", err)
	}
	if from != "" {
		l.rec.next(OutcomeTargetMoved)
		l.rec.t = started
		l.rec.pin(l.plan, &l.rec.a.Seconds.Rebase)
		printRebase(l.req.Out, l.plan, res)
		if err := l.recheck(ctx, from); err != nil {
			return nil, false, err
		}
	}
	landed, err = promote(ctx, l.repo, l.plan)
	l.rec.stage(&l.rec.a.Seconds.Promote)
	return landed, errors.Is(err, errTargetMoved), err
}

// promoteLoop promotes the verified lane; a moved target re-integrates and re-checks, up to maxRevalRounds times.
func (l *relander) promoteLoop(ctx context.Context) (*LandResult, error) {
	var landed *LandResult
	var err error
	for range maxRevalRounds + 1 {
		var again bool
		if landed, again, err = l.round(ctx); !again {
			break
		}
		l.rec.next(OutcomeTargetMoved)
	}
	if err == nil {
		return landed, nil
	}
	if landed != nil && landed.ResumeHint != "" {
		fmt.Fprintln(l.req.Err, "  retry:", landed.ResumeHint)
	}
	return nil, fmt.Errorf("promote: %w", err)
}
