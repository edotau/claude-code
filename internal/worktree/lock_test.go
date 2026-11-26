package worktree

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// A second land waits in line with a named holder and gives up with the "another land is running" line.
func TestWithLandRunWaitsInLine(t *testing.T) {
	repo, _, _ := laneLandFixture(t)
	oldWait, oldPoll := maxLandWait, pollInterval
	t.Cleanup(func() { maxLandWait, pollInterval = oldWait, oldPoll })
	maxLandWait, pollInterval = 30*time.Millisecond, 5*time.Millisecond

	var out bytes.Buffer
	var waitErr error
	ran, err := paths.TryFileLock(operationLeasePath(repo.Main, landRunName), 0, func() error {
		waitErr = withLandRun(t.Context(), repo, "b", &out, func(time.Duration) error { return nil })
		return nil
	})
	if err != nil || !ran {
		t.Fatalf("hold the lease: ran=%v err=%v", ran, err)
	}
	if waitErr == nil || !strings.Contains(waitErr.Error(), "another land is running") {
		t.Fatalf("want an another-land-is-running error, got %v", waitErr)
	}
	if !strings.Contains(out.String(), "waiting in line") {
		t.Fatalf("want the waiting line, got %q", out.String())
	}
	got := false
	if err := withLandRun(t.Context(), repo, "b", &out, func(time.Duration) error { got = true; return nil }); err != nil || !got {
		t.Fatalf("free lease: ran=%v err=%v", got, err)
	}
}
