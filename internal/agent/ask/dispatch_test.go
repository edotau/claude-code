package ask

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/roles"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// Given a detached leg whose spawn fails (the test binary is never relaunched), Then its record leaves running.
func TestDispatchDetachSpawnFailureFinishesRecord(t *testing.T) {
	vaulttest.UseFreshKey(t)
	legs := []roles.Leg{{Label: "a", Opts: agent.RunOptions{Agent: "claude", Prompt: "p"}}}
	var code int
	hosttest.CaptureOutput(t, func() int {
		code = dispatchDetach(context.Background(), agent.NewClient(nil, nil, nil), legs, helptext.Text)
		return code
	})
	if code != 1 {
		t.Fatalf("dispatchDetach = %d, want 1", code)
	}
	recs := agent.RecentRecords(5)
	if len(recs) != 1 || recs[0].Status != "error" || recs[0].Error == "" {
		t.Errorf("records = %+v, want one errored record with the cause", recs)
	}
}

func TestUnsupportedKeyNamesTheKeyDispatchCannotHonour(t *testing.T) {
	for key, ov := range map[string]agentconfig.Overlay{"model": {Model: "m"}, "effort": {Effort: "high"}, "host": {Host: "h"}, "": {Profile: "dev"}} {
		if got := unsupportedKey(ov); got != key {
			t.Errorf("unsupportedKey(%+v) = %q, want %q", ov, got, key)
		}
	}
}

// An empty overlay names nothing; one with several unsupported keys still names one of them, never "".
func TestUnsupportedKeyEmptyAndSeveral(t *testing.T) {
	for name, tc := range map[string]struct {
		ov   agentconfig.Overlay
		want []string
	}{
		"empty":   {agentconfig.Overlay{}, []string{""}},
		"two":     {agentconfig.Overlay{Effort: "high", Host: "h"}, []string{"effort", "host"}},
		"all":     {agentconfig.Overlay{Model: "m", Effort: "high", Host: "h"}, []string{"model", "effort", "host"}},
		"allowed": {agentconfig.Overlay{Profile: "dev", Fallback: "none"}, []string{""}},
	} {
		if got := unsupportedKey(tc.ov); !slices.Contains(tc.want, got) {
			t.Errorf("%s: unsupportedKey = %q, want one of %q", name, got, tc.want)
		}
	}
}

func TestDispatchReportWriteFailureExitsOne(t *testing.T) {
	vaulttest.UseFreshKey(t)
	vaulttest.SetRecord(t, "codex", "profile", "chatgpt")
	pathstest.FakeVendor(t, "codex", `[ "$1" = login ] && { echo "Logged in using ChatGPT"; exit 0; }`+"\ncat >/dev/null; echo successful leg")
	full, err := os.OpenFile("/dev/full", os.O_WRONLY, 0)
	if os.IsNotExist(err) {
		t.Skip("requires /dev/full")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := full.Close(); err != nil {
			t.Error(err)
		}
	})
	for _, format := range []string{helptext.Text, helptext.JSON} {
		t.Run(format, func(t *testing.T) {
			checkDispatchReportWriteFailure(t, full, format)
		})
	}
}

// A consumer that stops draining ends the leg (and frees the gate) instead of blocking its send forever.
func TestStreamLegAbandonedConsumerReleasesGate(t *testing.T) {
	armRateFixture(t, nil, nil)
	orig := armStreamStall
	armStreamStall = 50 * time.Millisecond
	t.Cleanup(func() { armStreamStall = orig })
	a := &armRetry{agentName: "codex", fallback: router.FallbackNone, tried: []string{"dev"}}
	o := agent.RunOptions{Agent: "codex", Prompt: "go", Fallback: router.FallbackNone}
	done := make(chan struct{})
	go func() {
		a.streamLeg(t.Context(), newClient(), o, make(chan wire.Event)) // unbuffered, never read
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("streamLeg still blocked on an undrained channel")
	}
}

func checkDispatchReportWriteFailure(t *testing.T, full *os.File, format string) {
	t.Helper()
	role := roles.Role{Name: "explore", Agent: "codex"} // explore refuses opencode and gemini (prompt-only read-only)
	request := roles.Request{Task: "test report", Parts: []string{"test leg"}, WorkDir: t.TempDir()}
	run := func() int { return dispatchRun(t.Context(), role, request, 1, format, false) }
	code, stdout, stderr := hosttest.CaptureOutput(t, run)
	if code != 0 || !strings.Contains(stdout, "successful leg") {
		t.Fatalf("successful dispatch control: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	code, _, stderr = hosttest.CaptureOutput(t, func() int {
		original := os.Stdout
		os.Stdout = full
		defer func() { os.Stdout = original }()
		return run()
	})
	if code != 1 || !strings.Contains(stderr, "dispatch: write report:") || !strings.Contains(stderr, "no space left on device") {
		t.Fatalf("lost report: exit %d, stderr %q; want exit 1 naming report write failure", code, stderr)
	}
}
