package serve

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestQueueWatchPools(t *testing.T) {
	now := time.Now()
	status := Status{Pools: []PoolRow{
		{Pool: "qa", Waiting: []Waiter{{ID: "a", Since: now.Add(-3 * time.Second)}, {ID: "b", Since: now.Add(-time.Second)}}},
		{Pool: "dev", Waiting: []Waiter{{ID: "c", Since: now}}},
	}}
	got := queueWatch(status, nil, now)
	if got.Gone || got.Limit != requeueWindow || len(got.Jobs) != 3 {
		t.Fatalf("snapshot = %+v", got)
	}
	if got.Jobs["b"].Ahead != 1 || got.Jobs["b"].Pool != "qa" || got.Jobs["a"].Wait != 3*time.Second || got.Jobs["c"].Ahead != 0 {
		t.Fatalf("jobs = %+v", got.Jobs)
	}
}

func TestQueueWatchAbsence(t *testing.T) {
	for _, c := range []struct {
		err  error
		gone bool
	}{
		{ErrNotRunning, true}, {os.ErrNotExist, true}, {syscall.ECONNREFUSED, true},
		{context.DeadlineExceeded, false}, {os.ErrPermission, false},
	} {
		got := queueWatch(Status{}, c.err, time.Now())
		if got.Gone != c.gone || got.Limit != requeueWindow || len(got.Jobs) != 0 {
			t.Fatalf("%v: %+v", c.err, got)
		}
	}
}
