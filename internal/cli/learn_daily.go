package cli

import (
	"fmt"
	"io"
	"time"

	"github.com/edotau/claude-code/internal/catalog"
	"github.com/edotau/claude-code/internal/memory"
)

const learnEvery = 24 * time.Hour

// Seams for the two learn steps; tests replace them.
var (
	learnRouting = func(now time.Time) error { _, err := foldFit(now, false); return err }
	learnRecall  = memory.LearnRecall
)

// dailyLearn runs both learn steps once a day in the detached catch-up child; a failure logs and retries next day.
func dailyLearn(now time.Time, errw io.Writer) {
	if !catalog.ClaimEvery("learn-daily", learnEvery, now) {
		return
	}
	if err := learnRouting(now); err != nil {
		fmt.Fprintf(errw, "learn: routing fit: %v\n", err)
	}
	if err := learnRecall(now); err != nil {
		fmt.Fprintf(errw, "learn: recall weights: %v\n", err)
	}
}
