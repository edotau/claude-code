package memhook

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/memory"
)

// Compact (PreCompact) runs the memory update and the generated handoff side by side within budget; never blocks.
func Compact(in *core.Input) core.Result {
	in = AgentInput(sessionRootInput(in))
	ctx, cancel := compactContext()
	defer cancel()
	now, update, handoff := time.Now(), compactUpdate, compactHandoff // read once: the goroutines share no mutable state
	var wg sync.WaitGroup
	wg.Go(func() { guarded("memory update", func() { update(in, now) }) })
	wg.Go(func() { guarded("generated handoff", func() { handoff(ctx, in, now) }) })
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		fmt.Fprintln(os.Stderr, "memory-compact: budget reached; proceeding while work still runs")
	}
	return core.Result{}
}

// guarded runs fn, turning a panic into a stderr note: a goroutine's panic would otherwise kill the hook.
func guarded(what string, fn func()) {
	defer func() {
		if p := recover(); p != nil {
			fmt.Fprintf(os.Stderr, "memory-compact: %s: panic: %v\n", what, p)
		}
	}()
	fn()
}

// Seams for tests: the hook budget, both halves of Compact and the dispatch records.
var (
	compactContext = func() (context.Context, context.CancelFunc) { return core.HookContext("memory-compact") }
	compactUpdate  = updateBeforeCompact
	compactHandoff = writeGeneratedHandoff
	recentRecords  = agent.RecentRecords
)

// updateBeforeCompact updates any work no earlier dispatch covered and leaves the compaction record.
func updateBeforeCompact(in *core.Input, now time.Time) {
	if !memory.Updatable(in.SessionID, in.TranscriptPath) {
		return
	}
	pct, _ := contextPct(in.SessionID, in.TranscriptPath)
	memory.WriteCompactRecord(in.SessionID, in.Trigger, pct, now)
	if g := memory.UpdateIfNew(updateRequest(in), memory.TriggerPreCompact, now); g.FailedOpen {
		fmt.Fprintln(os.Stderr, "memory-compact: memory update not dispatched; SessionEnd retries")
	}
}

// writeGeneratedHandoff writes the session's fact-only handoff; a failure is a stderr note.
func writeGeneratedHandoff(ctx context.Context, in *core.Input, now time.Time) {
	h := memory.CompactHandoff{SessionID: in.SessionID, CWD: in.CWD, Trigger: in.Trigger, Dispatches: sessionDispatches(), Now: now}
	if _, err := memory.WriteCompactHandoff(ctx, h); err != nil {
		fmt.Fprintf(os.Stderr, "memory-compact: generated handoff: %v\n", err)
	}
}

// sessionDispatches are the dispatch records this session launched, matched by owner pid (sessionOwner).
func sessionDispatches() []memory.HandoffDispatch {
	owner := sessionOwner()
	if owner <= 1 {
		return nil
	}
	var out []memory.HandoffDispatch
	for _, r := range recentRecords(agent.AllRecords) {
		if r.Owner != owner {
			continue
		}
		d := memory.HandoffDispatch{ID: r.ID, Agent: cmp.Or(r.RanAs, r.Agent), Status: r.Status, Brief: r.Request.Prompt,
			Model: cmp.Or(r.Model, r.Invoked, r.Request.Model), Started: r.Time}
		if !r.Alive() {
			d.Done = r.Time.Add(time.Duration(r.DurationMs) * time.Millisecond)
			if d.Status == "running" {
				d.Status = "dead"
			}
		}
		out = append(out, d)
	}
	return out
}
