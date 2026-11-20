package paths

import (
	"path/filepath"
	"testing"
)

func TestMemoryStoreLayout(t *testing.T) {
	store := MemoryStoreDir()
	if want := filepath.Join(ConfigDir(), "memory"); store != want {
		t.Fatalf("MemoryStoreDir = %q, want %q", store, want)
	}
	if got, want := MemoryStateDir(), filepath.Join(StateDir(), "memory"); got != want {
		t.Fatalf("MemoryStateDir = %q, want %q", got, want)
	}
	if got, want := MemorySub("reports", "a/b.md"), filepath.Join(store, "reports", "a", "b.md"); got != want {
		t.Fatalf("MemorySub = %q, want %q", got, want)
	}
	if MemorySubDir("x") != filepath.Join(store, "x") {
		t.Fatal("MemorySubDir")
	}
}
