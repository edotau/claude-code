package providers

import (
	"os"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
)

// Given a v1 file (one entry per workspace), decoding yields that entry under "claude".
func TestLastProfileMigratesV1ToClaude(t *testing.T) {
	raw := []byte(`{"schema":1,"workspaces":{"/w/a":{"profile":"prod","updated":"2026-01-02T03:04:05Z"}}}`)
	var st lastProfiles
	if _, err := schemas.Decode(lastProfileFormat, raw, &st); err != nil {
		t.Fatal(err)
	}
	if got := st.Workspaces["/w/a"]["claude"].Profile; got != "prod" || len(st.Workspaces["/w/a"]) != 1 {
		t.Errorf("v1 entry: got %+v, want claude=prod only", st.Workspaces)
	}
}

// Given the last-profile lock is held by a writer, ClearLastProfiles waits for it instead of racing it.
func TestClearLastProfilesTakesFileLock(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	RecordLastProfile("claude", "prod")
	done := make(chan error, 1)
	err := paths.WithFileLock(lastProfileFile(), func() error {
		go func() { done <- ClearLastProfiles() }()
		select {
		case <-done:
			t.Error("ClearLastProfiles returned while the lock was held")
		case <-time.After(200 * time.Millisecond):
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(lastProfileFile()); !os.IsNotExist(err) {
		t.Errorf("last-profile.json still present after clear: %v", err)
	}
}
