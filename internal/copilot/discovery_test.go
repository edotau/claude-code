package copilot

import (
	"testing"
	"time"
)

// An invalid HARNESS_MODEL_CACHE_TTL falls back to the default; a valid one wins.
func TestCopilotModelCacheTTL(t *testing.T) {
	for raw, want := range map[string]time.Duration{"": copilotSnapshotTTL, "bogus": copilotSnapshotTTL, "-1h": copilotSnapshotTTL, "90m": 90 * time.Minute} {
		t.Setenv(copilotCacheTTLEnv, raw)
		if got := copilotModelCacheTTL(); got != want {
			t.Errorf("%s=%q: ttl = %s, want %s", copilotCacheTTLEnv, raw, got, want)
		}
	}
}
