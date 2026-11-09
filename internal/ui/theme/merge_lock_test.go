package theme

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/edotau/claude-code/internal/paths"
)

func TestMergeFileHoldsDestinationLock(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "config.json")
	initial := []byte(`{"theme":"old","model":"keep"}`)
	if err := os.WriteFile(dest, initial, 0o600); err != nil {
		t.Fatal(err)
	}
	previous := beforeWrite
	t.Cleanup(func() { beforeWrite = previous })
	called := false
	beforeWrite = func(path string) {
		called = true
		lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if paths.TryLock(lock) {
			t.Error("merge does not hold destination lock; a concurrent writer can overwrite the checked contents")
		}
		if err := lock.Close(); err != nil {
			t.Fatal(err)
		}
	}

	changed, backup, err := mergeFile(dest, map[string]any{"theme": "new"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !called || !changed || backup == "" {
		t.Fatalf("hook called = %v, changed = %v, backup = %q", called, changed, backup)
	}
	assertMergeLockFileContents(t, backup, initial)
	assertMergeLockFileContents(t, dest, []byte("{\n  \"model\": \"keep\",\n  \"theme\": \"new\"\n}\n"))
}

func assertMergeLockFileContents(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}
