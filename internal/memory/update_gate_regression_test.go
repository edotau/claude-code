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

// Six user turns and zero edits block a failed Stop with 3 work units, at the first block and the re-block.
func TestFailedStopGateCountsWorkUnits(t *testing.T) {
	u := newGateRun(t)
	hosttest.WriteUserTurns(t, u.tr, 6)
	StubSpawn(t, false)
	for range 2 {
		if g := u.stop(); !g.Blocked || g.Work != 3 {
			t.Fatalf("six user turns and zero edits: gate=%+v, want block with 3 work units", g)
		}
	}
}
