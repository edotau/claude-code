package hooks

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

func TestHookMainRejectsStaleMainStamp(t *testing.T) {
	dir := t.TempDir()
	stamp := filepath.Join(dir, paths.StampFile)
	main := filepath.Join(dir, "claude-code")
	old := paths.BuildStamp
	paths.BuildStamp = "same-stamp"
	t.Cleanup(func() { paths.BuildStamp = old })
	if err := os.WriteFile(stamp, []byte(paths.BuildStamp), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(main, []byte("new main"), 0o755); err != nil {
		t.Fatal(err)
	}
	before := time.Now().Add(-time.Hour)
	if err := os.Chtimes(stamp, before, before); err != nil {
		t.Fatal(err)
	}
	if stampMatches(dir) {
		t.Fatal("old stamp accepted after main binary was replaced")
	}
	after := time.Now()
	if err := os.Chtimes(stamp, after, after); err != nil {
		t.Fatal(err)
	}
	if !stampMatches(dir) {
		t.Fatal("matching stamp newer than main was rejected")
	}
}
