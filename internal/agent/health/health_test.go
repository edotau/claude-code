package health

import (
	"testing"
	"time"
)

// A clear removes the outage it observed, never a newer one; a budget end degrades its task at once.
func TestClearDownIsGenerationBound(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	Degrade(ProxyDown)
	old := read().Down
	ClearDown()
	time.Sleep(2 * time.Millisecond)
	Degrade(ProxyUnhealthy)
	clearObserved(old) // the delayed clear of the first outage
	if got := Of("k"); !got.Degraded || got.Reason != ProxyUnhealthy {
		t.Fatalf("the newer outage was erased: %+v", got)
	}
	Fail("k", "plan 1", "time")
	if got := Of("k"); got.Reason != Budget("time") || got.Attempts != 1 {
		t.Fatalf("budget end: %+v", got)
	}
}
