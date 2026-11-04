package router

import (
	"os"
	"testing"
)

// An unreadable quota.json (a directory here) must fail Update and stay untouched, not read as empty.
func TestUpdateRefusesUnreadableState(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	path := StatePath()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	called := false
	if err := Update(func(State) { called = true }); err == nil || called {
		t.Fatalf("Update must fail before applying fn on an unreadable file: err=%v called=%v", err, called)
	}
	if fi, err := os.Stat(path); err != nil || !fi.IsDir() {
		t.Fatalf("state path changed: %v %v", fi, err)
	}
}
