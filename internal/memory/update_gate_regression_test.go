package memory

import (
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
)

func TestFailedStopDispatchLeavesWorkForFinalHook(t *testing.T) {
	for _, trigger := range []string{TriggerSessionEnd, TriggerPreCompact} {
		t.Run(trigger, func(t *testing.T) {
			u := newGateRun(t)
			hosttest.WriteEdits(t, u.tr, 5)
			StubSpawn(t, false)
			if g := u.stop(); !g.Blocked {
				t.Fatalf("failed Stop dispatch gate=%+v, want block", g)
			}
			if st, ok := readStamp(stampPath("s1")); !ok || st.edits != 0 {
				t.Errorf("failed dispatch covered work: stamp=%+v exists=%v", st, ok)
			}
			calls := &StubSpawn(t, true).Update
			runGate("s1", u.tr, u.cwd, trigger)
			if calls.Load() != 1 {
				t.Fatalf("%s skipped retry after failed Stop: spawns=%d", trigger, calls.Load())
			}
			if st, ok := readStamp(stampPath("s1")); !ok || st.state != "bg" || st.edits != 5 {
				t.Fatalf("successful retry stamp=%+v exists=%v, want bg covering 5 edits", st, ok)
			}
		})
	}
}

// A Stop after a failed spawn retries it; success heals silently and later Stops never spawn the same work again.
func TestFailedStopSpawnRetriesAtNextStop(t *testing.T) {
	u := newGateRun(t)
	hosttest.WriteEdits(t, u.tr, 5)
	StubSpawn(t, false)
	if g := u.stop(); !g.Blocked {
		t.Fatalf("failed spawn gate=%+v, want the not-saved note", g)
	}
	calls := &StubSpawn(t, true).Update
	for i := range 2 {
		if g := u.stop(); g.Blocked || g.FailedOpen {
			t.Fatalf("Stop %d after recovery gate=%+v, want silent", i, g)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("spawns after recovery=%d, want exactly 1 retry", calls.Load())
	}
	if st, ok := readStamp(stampPath("s1")); !ok || st.state != "bg" || st.edits != 5 {
		t.Fatalf("retry stamp=%+v exists=%v, want bg covering 5 edits", st, ok)
	}
}

// Six user turns and zero edits block a failed Stop with 3 work units, at the first block and the re-block.
func TestFailedStopGateCountsWorkUnits(t *testing.T) {
	u := newGateRun(t)
	hosttest.WriteUserTurns(t, u.tr, 6)
	StubSpawn(t, false)
	for range 2 {
		if g := u.stop(); !g.Blocked {
			t.Fatalf("six user turns and zero edits: gate=%+v, want block", g)
		}
	}
}
