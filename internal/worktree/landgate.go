package worktree

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"
)

var (
	gateSlotWait = 10 * time.Minute
	gateSlotPoll = 15 * time.Second
)

// HeldGate is a warm slot held for a land's Go gate; a nil *HeldGate means gate in the lane.
type HeldGate struct {
	dir  string
	slot *Slot
}

// usesGateSlot reports whether the gate is a go build/vet/test gate that benefits from a warm slot.
func usesGateSlot(gate, source string) bool {
	return gate == fullGoGate || strings.HasPrefix(source, scopedGateSource)
}

// HoldGate acquires a slot at oid, waiting up to 10 min while all are busy; nil on any failure (gate in the lane).
func HoldGate(ctx context.Context, top, oid, lane string, out io.Writer) *HeldGate {
	deadline := time.Now().Add(gateSlotWait)
	for warned := false; ; warned = true {
		s, err := TryAcquireSlot(ctx, top, oid)
		if err != nil {
			fmt.Fprintf(out, "land %s: no gate slot (%v); gating in the lane\n", lane, err)
			return nil
		}
		if s != nil {
			return &HeldGate{dir: s.Dir, slot: s}
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return nil
		}
		if !warned {
			fmt.Fprintf(out, "land %s: waiting for a gate slot (%d busy)\n", lane, slotCount)
		}
		select {
		case <-ctx.Done():
		case <-time.After(gateSlotPoll):
		}
	}
}

// Where is the dir to gate in: the slot, or laneDir when nothing is held.
func (h *HeldGate) Where(laneDir string) string {
	if h == nil {
		return laneDir
	}
	return h.dir
}

// At is where the plan's gate runs: the slot, or the lane when nothing is held.
func (h *HeldGate) At(plan *LandPlan) gateAt {
	return gateAt{Dir: h.Where(plan.WorktreePath), Base: plan.TargetOID, Lane: plan.WorktreePath}
}

// Reset moves a held slot to oid for the next revalidation round; a nil gate has nothing to reset.
func (h *HeldGate) Reset(ctx context.Context, oid string) error {
	if h == nil {
		return nil
	}
	return h.slot.Reset(ctx, oid)
}

// Release hands the slot back; safe on nil.
func (h *HeldGate) Release() {
	if h != nil {
		h.slot.Release()
	}
}
