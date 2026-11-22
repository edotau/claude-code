package waves

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/plans"
)

// wait is what one leaf waits on: siblings of its own wave, done earlier tasks, or a hold only main can lift.
type wait struct {
	after   []string // same-wave leaf ids that must finish ok first
	earlier []string // earlier-wave or ticked blockers already done; shown by the dry run
	hold    string   // non-empty: never launched, needs main
}

func specID(id string) string { return strings.TrimPrefix(id, "Phase ") }

// waits maps each launched leaf of wave w that names a Blocked by to what it still waits on.
func (r *runner) waits(w plans.Wave) map[string]wait {
	tasks := map[string]plans.WaveTask{}
	for _, wv := range r.plan.Waves {
		for _, t := range wv.Tasks {
			tasks[t.ID] = t
		}
	}
	for _, t := range r.plan.Manual {
		tasks[t.ID] = t
	}
	done := r.verifiedLeaves()
	out := map[string]wait{}
	for _, t := range w.Tasks {
		if !t.Manual && (len(t.BlockedBy) > 0 || t.BlockedText != "") {
			out[specID(t.ID)] = r.taskWait(t, tasks, done)
		}
	}
	return out
}

// verifiedLeaves are the leaves this run's state already judged ok, in any wave.
func (r *runner) verifiedLeaves() map[string]bool {
	done := map[string]bool{}
	for _, ws := range r.state.Waves {
		for _, l := range ws.Leaves {
			done[l.ID] = done[l.ID] || l.Verdict == LeafStateOK
		}
	}
	return done
}

// barrierDone is a dry run's view of an earlier-wave blocker: the requested range runs its wave first.
func (r *runner) barrierDone(b, t plans.WaveTask) bool {
	return r.o.DryRun && !b.Manual && b.Wave < t.Wave && r.selected(b.Wave)
}

// taskWait sorts t's blockers: ticked or verified ones are done, open siblings are waited on, the rest hold.
func (r *runner) taskWait(t plans.WaveTask, tasks map[string]plans.WaveTask, done map[string]bool) wait {
	var wt wait
	var held []string
	if t.BlockedText != "" {
		held = append(held, t.BlockedText)
	}
	for _, id := range t.BlockedBy {
		b, short := tasks[id], specID(id)
		switch {
		case b.Done:
			wt.earlier = append(wt.earlier, short+" (ticked)")
		case b.Wave == t.Wave && !b.Manual:
			wt.after = append(wt.after, short)
		case done[short] || r.barrierDone(b, t):
			wt.earlier = append(wt.earlier, short)
		default:
			held = append(held, short+" not done")
		}
	}
	if len(held) > 0 {
		wt.hold = "needs main: " + strings.Join(held, "; ")
	}
	return wt
}

// blocked returns why a leaf can never start, else whether every sibling it waits on finished ok.
func blocked(w wait, status map[string]string) (string, bool) {
	if w.hold != "" {
		return w.hold, false
	}
	for _, id := range w.after {
		switch status[id] {
		case LeafStateOK:
		case "":
			return "", false
		default:
			return "blocker " + id + " failed", false
		}
	}
	return "", true
}

// nextRound splits pending into leaves ready now, leaves still waiting and skipped ones, marking skips failed.
func nextRound(pending []Spec, waits map[string]wait, status map[string]string) (ready, rest []Spec, skipped []Envelope) {
	for _, s := range pending {
		reason, ok := blocked(waits[s.ID], status)
		switch {
		case reason != "":
			status[s.ID] = LeafStateSkipped
			skipped = append(skipped, Envelope{TaskID: s.ID, Status: LeafStateSkipped, Blocked: reason})
		case ok:
			ready = append(ready, s)
		default:
			rest = append(rest, s)
		}
	}
	if len(ready)+len(skipped) == 0 { // unreachable after the parse refuses cycles; never spin
		for _, s := range rest {
			status[s.ID] = LeafStateSkipped
			skipped = append(skipped, Envelope{TaskID: s.ID, Status: LeafStateSkipped, Blocked: "a blocker never finished"})
		}
		rest = nil
	}
	return ready, rest, skipped
}

// launchRounds launches the wave in rounds: a leaf starts once every sibling it waits on finished ok.
func (r *runner) launchRounds(ctx context.Context, launch, wave []Spec, waits map[string]wait,
	checkpoint func([]Envelope) error) ([]Envelope, error) {
	status := map[string]string{}
	for _, s := range wave { // a leaf not relaunched was kept: its dispatch already finished ok
		status[s.ID] = LeafStateOK
	}
	for _, s := range launch {
		delete(status, s.ID)
	}
	var out []Envelope
	var runErr error
	for pending := launch; len(pending) > 0; {
		ready, rest, skipped := nextRound(pending, waits, status)
		for i := range skipped {
			skipped[i].TaskID = r.slug + "-" + skipped[i].TaskID
		}
		out = append(out, skipped...)
		if err := checkpoint(skipped); err != nil {
			return out, errors.Join(runErr, err)
		}
		pending = rest
		if len(ready) == 0 {
			continue
		}
		envelopes, err := r.launch(ctx, ready, wave)
		runErr = errors.Join(runErr, err)
		for _, s := range ready {
			status[s.ID] = "failed"
		}
		for _, e := range envelopes {
			if e.Status == LeafStateOK {
				status[strings.TrimPrefix(e.TaskID, r.slug+"-")] = LeafStateOK
			}
		}
		out = append(out, envelopes...)
		if err := checkpoint(envelopes); err != nil {
			return out, errors.Join(runErr, err)
		}
		if ctx.Err() != nil {
			break
		}
	}
	return out, runErr
}

// dryOrder labels each leaf with what it waits on and orders the wave as the rounds would launch it.
func dryOrder(specs []Spec, waits map[string]wait) []Spec {
	for i, s := range specs {
		w := waits[s.ID]
		var on []string
		on = append(append(on, w.earlier...), w.after...)
		switch {
		case w.hold != "":
			specs[i].Waits = "held: " + w.hold
		case len(on) > 0:
			specs[i].Waits = "waits on " + strings.Join(on, ",")
		}
	}
	status := map[string]string{}
	var out, held []Spec
	for pending := specs; len(pending) > 0; {
		ready, rest, skipped := nextRound(pending, waits, status)
		for _, s := range ready {
			status[s.ID] = LeafStateOK
		}
		for _, e := range skipped {
			if i := slices.IndexFunc(specs, func(s Spec) bool { return s.ID == e.TaskID }); i >= 0 {
				specs[i].Waits = "held: " + e.Blocked
				held = append(held, specs[i])
			}
		}
		out, pending = append(out, ready...), rest
	}
	return append(out, held...)
}
