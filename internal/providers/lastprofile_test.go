package providers

import (
	"os"
	"path/filepath"
	"strings"
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

func TestConfigFixRecordLastProfileWarnsOnPersistenceFailure(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	state := filepath.Join(dir, "state")
	writeTestFile(t, state, "not a directory", 0o600)
	stderr := captureConfigFixStderr(t, func() { RecordLastProfile("claude", "work") })
	for _, want := range []string{"warning: last profile for claude not saved:", state, "not a directory"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr = %q, want containing %q", stderr, want)
		}
	}
}

func captureConfigFixStderr(t *testing.T, fn func()) string {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	old := os.Stderr
	os.Stderr = file
	defer func() { os.Stderr = old }()
	fn()
	data, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
