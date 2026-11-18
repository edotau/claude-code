package cli

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

func TestDailyLearnRunsOncePerDayAndSwallowsFailure(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	var routing, recall int
	origR, origC := learnRouting, learnRecall
	t.Cleanup(func() { learnRouting, learnRecall = origR, origC })
	learnRouting = func(time.Time) error { routing++; return errors.New("boom") }
	learnRecall = func(time.Time) error { recall++; return nil }
	var errw bytes.Buffer
	now := time.Now()
	dailyLearn(now, &errw)
	if routing != 1 || recall != 1 || errw.Len() == 0 {
		t.Fatalf("due: routing=%d recall=%d log=%q, want 1, 1 and a logged failure", routing, recall, errw.String())
	}
	dailyLearn(now.Add(time.Hour), &errw)
	if routing != 1 || recall != 1 {
		t.Fatalf("not due: routing=%d recall=%d, want no further calls", routing, recall)
	}
	dailyLearn(now.Add(25*time.Hour), &errw)
	if routing != 2 {
		t.Fatalf("a day later: routing=%d, want 2", routing)
	}
}
