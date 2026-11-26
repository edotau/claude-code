package proxy

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// watch builds a FailWatch whose clock is *at, collecting reported events into *got.
func watch(at *time.Time, log string, got *[]FailureEvent) *FailWatch {
	fw := NewFailWatch(log, func(ev FailureEvent) bool {
		*got = append(*got, ev)
		return true
	})
	fw.now = func() time.Time { return *at }
	return fw
}

func TestFailWatchThreshold(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	var got []FailureEvent
	fw := watch(&now, "/tmp/proxy.log", &got)
	for range 10 {
		if fw.Record("databricks", 429) {
			t.Fatal("429 reported as proxy_5xx")
		}
	}
	for i := range fail5xxThreshold - 1 {
		if fw.Record("databricks", 502) {
			t.Fatalf("reported early at hit %d", i+1)
		}
		now = now.Add(5 * time.Second)
	}
	if !fw.Record("databricks", 502) {
		t.Fatal("expected report on 5th 5xx in 60s")
	}
	now = now.Add(5 * time.Second)
	if fw.Record("databricks", 502) {
		t.Fatal("expected duplicate within 60s window to be suppressed")
	}
	if fw.Record("databricks", 503) {
		t.Fatal("503 should not share 502 ring counter")
	}
	if len(got) != 1 || got[0].Kind != failKindProxy5xx || got[0].Subject != "databricks/502" || got[0].Log != "/tmp/proxy.log" {
		t.Fatalf("unexpected failure events: %+v", got)
	}
}

func TestFailWatchSpreadNeverReports(t *testing.T) {
	at := time.Date(2026, 11, 25, 13, 0, 0, 0, time.UTC)
	var got []FailureEvent
	fw := watch(&at, "", &got)
	for i := range fail5xxThreshold {
		if fw.Record("anthropic", 500) {
			t.Fatalf("spread-out 500s should not report (hit %d)", i+1)
		}
		at = at.Add(20 * time.Second)
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 spread events, got %+v", got)
	}
}

func TestFailWatchRetriesAfterRefusal(t *testing.T) {
	fw := NewFailWatch("/tmp/proxy.log", nil)
	at := time.Date(2026, 11, 25, 14, 0, 0, 0, time.UTC)
	fw.now = func() time.Time { return at }
	if fw.Record("openai", 502) {
		t.Fatal("nil reporter must return false")
	}
	accept := false
	fw.report = func(FailureEvent) bool { return accept }
	for range fail5xxThreshold {
		if fw.Record("openai", 502) {
			t.Fatal("false reporter return must return false")
		}
	}
	accept = true
	if !fw.Record("openai", 502) {
		t.Fatal("expected retry to succeed after false reporter did not suppress window")
	}
}

func TestFailWatchConcurrentReportsOnce(t *testing.T) {
	var calls atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	fw := NewFailWatch("/tmp/proxy.log", func(FailureEvent) bool {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		return true
	})
	at := time.Date(2026, 11, 25, 15, 0, 0, 0, time.UTC)
	fw.now = func() time.Time { return at }
	for range fail5xxThreshold - 1 {
		fw.Record("gemini", 503)
	}
	var wg sync.WaitGroup
	wg.Go(func() { fw.Record("gemini", 503) })
	<-entered
	if fw.Record("gemini", 503) {
		t.Fatal("concurrent Record should be suppressed while report is in flight")
	}
	close(release)
	wg.Wait()
	if n := calls.Load(); n != 1 {
		t.Fatalf("concurrent reporter calls = %d, want 1", n)
	}
}

func TestEnsureReportsDeadDaemon(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	prevSpawn := spawnDaemon
	prevAlive := alive
	prevReporter := currentFailureReporter()
	t.Cleanup(func() {
		spawnDaemon = prevSpawn
		alive = prevAlive
		RegisterFailureReporter(prevReporter)
	})
	var got []FailureEvent
	RegisterFailureReporter(func(ev FailureEvent) bool {
		got = append(got, ev)
		return true
	})
	spawnErr := errors.New("stop after report check")
	spawnDaemon = func(int) error { return spawnErr }
	alive = func(int) bool { return false }

	if _, err := WriteState(State{PID: 424242, Port: 18765, Started: time.Now().UTC()}); err != nil {
		t.Fatalf("WriteState: %v", err)
	}
	logContent := "proxy: listening on http://127.0.0.1:18765 (pid 424242)\n" +
		"proxy: exiting (pid 424242): older exit\n" +
		"proxy: exiting (pid 424242): bind failed\n" +
		"proxy: exiting (pid 999999): unrelated exit\n"
	if err := os.WriteFile(logFile(), []byte(logContent), 0o600); err != nil {
		t.Fatalf("WriteFile(proxy.log): %v", err)
	}

	_, err := Ensure(context.Background())
	if !errors.Is(err, spawnErr) {
		t.Fatalf("Ensure err = %v, want %v", err, spawnErr)
	}

	if len(got) != 1 {
		t.Fatalf("got %d failures, want 1: %+v", len(got), got)
	}
	if got[0].Kind != failKindProxyDown || got[0].Subject != "pid/424242" {
		t.Fatalf("unexpected failure event: %+v", got[0])
	}
	if !strings.Contains(got[0].Summary, "proxy: exiting (pid 424242): bind failed") {
		t.Fatalf("summary = %q, want last exiting line", got[0].Summary)
	}
	if got[0].Time <= 0 {
		t.Fatalf("time = %d, want > 0", got[0].Time)
	}
}
