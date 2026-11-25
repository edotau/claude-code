package ask

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/contract"
)

// leavesPoll is the interval of the leaf watch; leavesGrace spares a leaf that has only just started.
var (
	leavesPoll  = 3 * time.Second
	leavesGrace = 10 * time.Second
)

// leavesSettle caps the wait for open leaves before the gate; leavesSettlePoll is its poll interval.
var (
	leavesSettle     = 2 * time.Minute
	leavesSettlePoll = time.Second
)

// resendTries is how many transient re-send errors a leaf survives before it fails owner-dead.
const resendTries = 3

type sendFunc func(ctx context.Context, holder string) (string, error)

// watchLeaves watches owner contract id's leaves while its run lasts; the returned func stops the watch.
func watchLeaves(ctx context.Context, id string) func() {
	if c, err := contract.Load(id); err != nil || !slices.Contains(c.Terms, contract.TermLeaves) {
		return func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	tries := map[string]int{}
	go func() {
		for t := time.NewTicker(leavesPoll); ; {
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
				watchOnce(ctx, id, agent.Resend, tries)
			}
		}
	}()
	return cancel // stop does not wait for an in-flight re-send: it completes detached and the new dispatch takes over
}

// watchOnce re-sends each lost leaf of owner once, to a new holder; a second death fails it owner-dead.
func watchOnce(ctx context.Context, owner string, send sendFunc, tries map[string]int) {
	c, err := contract.Load(owner)
	if err != nil || c.State() != contract.StateWorking {
		return
	}
	for _, id := range c.Children {
		if ctx.Err() != nil {
			return
		}
		k, err := contract.Load(id)
		if err != nil || k.State() != contract.StateWorking {
			continue
		}
		if !leafLost(k) {
			delete(tries, id) // it recovered: the next death starts its count afresh
			continue
		}
		resendLeaf(ctx, k, send, tries)
	}
}

// settleLeaves waits until no leaf in ids is working, ctx ends, or the cap passes (none when no budget is left).
func settleLeaves(ctx context.Context, ids []string) []string {
	cp := leavesSettle
	if dl, ok := ctx.Deadline(); ok {
		cp = min(cp, time.Until(dl)-settleGateTimeout)
	}
	if cp <= 0 {
		return workingOf(ids)
	}
	t := time.NewTimer(cp)
	defer t.Stop()
	tk := time.NewTicker(leavesSettlePoll)
	defer tk.Stop()
	for open := workingOf(ids); len(open) > 0; open = workingOf(ids) {
		select {
		case <-ctx.Done():
			return workingOf(ids)
		case <-t.C:
			return workingOf(ids)
		case <-tk.C:
		}
	}
	return nil
}

// workingOf lists the ids of ids whose contract is working.
func workingOf(ids []string) []string {
	var out []string
	for _, id := range ids {
		if k, err := contract.Load(id); err == nil && k.State() == contract.StateWorking {
			out = append(out, id)
		}
	}
	return out
}

// resendLeaf hands lost leaf k to a fresh dispatch once; a leaf already re-sent, or one that cannot be, fails.
func resendLeaf(ctx context.Context, k contract.Contract, send sendFunc, tries map[string]int) {
	holder := k.Holders[len(k.Holders)-1]
	if len(k.Holders) > 1 {
		failLeaf(k.ID, holder)
		return
	}
	next, err := send(ctx, holder)
	if err != nil && agent.ResendClaimed(holder) { // a recovery is starting: let it finish before judging the leaf
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "ask: leaf %s: re-send: %v\n", k.ID, err)
		if tries[k.ID]++; tries[k.ID] < resendTries {
			return // the next poll retries
		}
		failLeaf(k.ID, holder)
		return
	}
	delete(tries, k.ID)
	_, err = contract.Update(k.ID, func(k contract.Contract) (contract.Contract, error) {
		if k.State() != contract.StateWorking || k.Holders[len(k.Holders)-1] != holder {
			return k, nil
		}
		return k.Reassign(next)
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "ask: leaf %s: re-assign to %s: %v\n", k.ID, next, err)
	}
}

// failLeaf fails leaf id owner-dead when holder is still its working holder.
func failLeaf(id, holder string) {
	_, err := contract.Update(id, func(k contract.Contract) (contract.Contract, error) {
		if k.State() != contract.StateWorking || k.Holders[len(k.Holders)-1] != holder {
			return k, nil
		}
		return k.Fail(contract.ReasonOwnerDead)
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "ask: leaf %s: fail: %v\n", id, err)
	}
}

// leafLost reports whether k's current holder is no longer running; a record not yet written is spared the grace.
func leafLost(k contract.Contract) bool {
	rec, err := agent.LoadRecord(k.Holders[len(k.Holders)-1])
	if err != nil {
		return errors.Is(err, agent.ErrNoRecord) && contractAge(k.ID) > leavesGrace
	}
	if rec.Status == wire.StatusOK { // it finished: a failed settle is not a death, and a re-run would repeat its edits
		return false
	}
	return (rec.Status != "running" || !agent.RecordLive(rec)) && time.Since(rec.Time) > leavesGrace
}

func contractAge(id string) time.Duration {
	p, err := contract.Path(id)
	if err != nil {
		return 0
	}
	fi, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return time.Since(fi.ModTime())
}
