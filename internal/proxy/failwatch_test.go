package proxy

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

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
