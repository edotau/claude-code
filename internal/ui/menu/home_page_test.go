package menu

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/network/tailscale"
)

// resetTailscaleStatus empties the home page's Status cache for this test and restores its seams after.
func resetTailscaleStatus(t *testing.T) {
	t.Helper()
	origTTL, origFetch := tsStatusTTL, tsStatusFetch
	tsStatus.Lock()
	tsStatus.at = time.Time{}
	tsStatus.Unlock()
	t.Cleanup(func() {
		tsStatus.Lock()
		tsStatus.at = time.Time{}
		tsStatus.Unlock()
		tsStatusTTL, tsStatusFetch = origTTL, origFetch
	})
}

// TestTailscaleStatusCachedWithinTTL: a render and runRemote inside the TTL share one Status call; expiry refetches.
func TestTailscaleStatusCachedWithinTTL(t *testing.T) {
	resetTailscaleStatus(t)
	calls := 0
	tsStatusFetch = func(context.Context) (tailscale.Local, error) {
		calls++
		return tailscale.Local{DNSName: "box.tailxxxxx.ts.net."}, errors.New("stopped")
	}
	for range 3 {
		if _, err := tailscaleStatus(context.Background()); err == nil || err.Error() != "stopped" {
			t.Fatalf("cached err = %v, want the fetched error", err)
		}
	}
	if calls != 1 {
		t.Fatalf("Status calls within TTL = %d, want 1", calls)
	}
	tsStatusTTL = 0
	if st, _ := tailscaleStatus(context.Background()); calls != 2 || st.DNSName != "box.tailxxxxx.ts.net." {
		t.Errorf("after TTL: calls = %d, DNSName %q; want a refetch", calls, st.DNSName)
	}
}
