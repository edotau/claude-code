package agent

import (
	"time"

	"github.com/edotau/claude-code/internal/plans"
)

func init() {
	plans.DispatchFailed = func(k plans.TaskKey, err error) { NoteStartFailure(TaskKey{Text: k.Text, PlanTask: k.PlanTask}, err) }
	plans.DispatchHealth = func(k plans.TaskKey) plans.DispatchState {
		h := DispatchHealth(TaskKey{Text: k.Text, PlanTask: k.PlanTask})
		return plans.DispatchState{Degraded: h.Degraded, Reason: h.Reason, Attempts: h.Attempts}
	}
	plans.BatchRecord = func(id string, since time.Time) (string, bool) {
		for _, r := range RecordsSince(since) { // reaps a lost record, which counts its failure once
			if r.TaskID == id {
				return r.Status, true
			}
		}
		return "", false
	}
}
