package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths"
)

// Given --waves run from a repo subdirectory, Then the runner's Dir is the repo top, not the subdirectory.
func TestWavesDirResolvesRepoTopFromSubdir(t *testing.T) {
	repo := hosttest.Repo(t)
	sub := filepath.Join(repo, "internal", "agent")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)
	got, err := wavesDir(context.Background())
	if err != nil || paths.CanonDeep(got) != paths.CanonDeep(repo) {
		t.Fatalf("wavesDir() = %q, %v; want %q", got, err, repo)
	}
}
