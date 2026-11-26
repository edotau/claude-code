package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/router/jev"
)

// hangJev serves /v1/systemone only once the client leaves, so a shadow call stays mid-flight until cancel.
func hangJev(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	// cleanups run last-first: close connections before Close waits on the handler, so a failed check never hangs
	t.Cleanup(srv.Close)
	t.Cleanup(srv.CloseClientConnections)
	t.Setenv("HARNESS_JEV_URL", srv.URL)
}

// settledGoroutines is the process goroutine count once it holds steady across two samples.
func settledGoroutines(t *testing.T) int {
	t.Helper()
	last := runtime.NumGoroutine()
	for range 40 {
		runtime.GC()
		time.Sleep(25 * time.Millisecond)
		n := runtime.NumGoroutine()
		if n == last {
			return n
		}
		last = n
	}
	t.Fatalf("goroutine count never settled (last %d)", last)
	return last
}

// waitGoroutinesAbove returns once the count tops base, proving a shadow is in flight.
func waitGoroutinesAbove(t *testing.T, base int) {
	t.Helper()
	for end := time.Now().Add(time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if runtime.NumGoroutine() > base {
			return
		}
	}
	t.Fatalf("goroutines = %d, never rose above baseline %d: the shadow never started", runtime.NumGoroutine(), base)
}

// A cancelled dispatch's jev shadow exits with it instead of hanging the local server until its own timeout.
func TestStartJevShadowExitsOnDispatchCancel(t *testing.T) {
	hangJev(t)
	base := settledGoroutines(t)

	ctx, cancel := context.WithCancel(context.Background())
	id := "test-jev-cancel"
	startJev(ctx, id, "review the sync.Map changes in internal/agent")
	t.Cleanup(func() { stopJev(id) })
	waitGoroutinesAbove(t, base) // the shadow and its HTTP call are in flight
	if _, ok := jevPending.Load(id); !ok {
		t.Fatal("startJev kept no pending call")
	}

	cancel()
	waitGoroutines(t, base)
	if j := takeJev(id); j == nil || j.Error != jev.ClassTimeout {
		t.Errorf("takeJev after cancel = %+v, want timeout class", j)
	}
}

// Finishing a record stops its unanswered shadow: the result channel is nobody's reason to keep the HTTP call.
func TestTakeJevStopsAnUnansweredShadow(t *testing.T) {
	hangJev(t)
	base := settledGoroutines(t)

	id := "test-jev-take"
	startJev(context.Background(), id, "summarize the fallback changes")
	waitGoroutinesAbove(t, base)

	j := takeJev(id) // the run ended first: timeout class, and the in-flight call is cancelled
	if j == nil || j.Error != jev.ClassTimeout || j.Keyword != tierName("summarize the fallback changes") {
		t.Errorf("takeJev = %+v, want timeout with keyword %q", j, tierName("summarize the fallback changes"))
	}
	waitGoroutines(t, base)
}

// A background job's parent drops the pending shadow at spawn: the detached child starts one of its own.
func TestStopJevReleasesASpawnedJobsShadow(t *testing.T) {
	hangJev(t)
	base := settledGoroutines(t)

	id := "test-jev-spawn"
	startJev(context.Background(), id, "explain the job recovery path")
	waitGoroutinesAbove(t, base)

	stopJev(id)
	if _, ok := jevPending.Load(id); ok {
		t.Error("stopJev kept the pending entry")
	}
	waitGoroutines(t, base)
	if j := takeJev(id); j != nil {
		t.Errorf("takeJev after stopJev = %+v, want nil", j)
	}
}

func TestJevBriefStripsRecallAndMeta(t *testing.T) {
	in := "<memory-recall>\nsecret old context\n</memory-recall>\nTier: deep\nWorkdir: /tmp/x\nHARD CAP: 5\nFix typo in README.md\nInvestigate race under concurrent writes\n\n## Leaf rules\nNever commit"
	want := "Fix typo in README.md\nInvestigate race under concurrent writes"
	if got := jevBrief(in); got != want {
		t.Fatalf("jevBrief = %q, want %q", got, want)
	}
}
