package agent

import (
	"time"

	"github.com/edotau/claude-code/internal/dev/plans"
)

func init() {
	plans.BatchRecord = func(id string, since time.Time) (string, bool) {
		for _, r := range RecordsSince(since) {
			if r.TaskID == id {
				return r.Status, true
			}
		}
		return "", false
	}
	plans.FailedAttempts = func(planTask string) int {
		n := 0
		for _, r := range RecentRecords(failedAttemptsWindow) { // newest first: stop at the task's last success
			if r.PlanTask != planTask || r.Status == "running" {
				continue
			}
			if !r.Failed() {
				break
			}
			n++
		}
		return n
	}
}

// failedAttemptsWindow is how many recent records the automatic fill reads to count a task's failures in a row.
const failedAttemptsWindow = 200
