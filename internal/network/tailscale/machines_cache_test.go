package tailscale

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeCacheAt plants a cache file whose timestamp is at, in a hermetic config dir.
func writeCacheAt(t *testing.T, at time.Time) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	data, err := json.Marshal(machinesCache{Schema: machinesCacheSchema, At: at, Names: []string{"h1"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(machinesCachePath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(machinesCachePath(), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A fresh entry is served; one stamped in the future (clock skew) is stale, never immortal.
func TestCachedMachineNamesFreshnessWindow(t *testing.T) {
	writeCacheAt(t, time.Now().Add(-time.Minute))
	if got := CachedMachineNames(); len(got) != 1 {
		t.Errorf("fresh cache = %v, want [h1]", got)
	}
	writeCacheAt(t, time.Now().Add(24*time.Hour))
	if got := CachedMachineNames(); got != nil {
		t.Errorf("future-dated cache = %v, want nil", got)
	}
}
