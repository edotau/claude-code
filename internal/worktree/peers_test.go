package worktree

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths"
)

// A nested worktree never skips the main tree, which holds all its peers.
func TestCheckoutPeersInfoPassesOnlyNestedWorktrees(t *testing.T) {
	main := paths.Real(hosttest.Repo(t))
	nested := filepath.Join(main, ".wt", "x")
	hosttest.In(t, main, "worktree", "add", "-q", "-b", "x", nested)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if top, _, skip := CheckoutPeersInfo(ctx, nested); top != nested || len(skip) != 0 {
		t.Fatalf("from the nested worktree: top %q, skip %v; want top %q and no skip dirs", top, skip, nested)
	}
	if top, branch, skip := CheckoutPeersInfo(ctx, main); top != main || branch == "" || len(skip) != 1 || skip[0] != nested {
		t.Fatalf("from the main tree: top %q, branch %q, skip %v; want top %q, a branch and skip [%s]", top, branch, skip, main, nested)
	}
}

func TestCheckoutPeersInfoReadsMetadataWithoutGit(t *testing.T) {
	repo := paths.Real(hosttest.Repo(t))
	nested := filepath.Join(repo, "nested")
	hosttest.In(t, repo, "worktree", "add", "-q", "-b", "topic", nested)
	head := strings.TrimSpace(hosttest.In(t, repo, "rev-parse", "HEAD"))
	hosttest.In(t, repo, "pack-refs", "--all", "--prune")
	t.Setenv("PATH", t.TempDir())
	for _, tc := range []struct {
		name, head, branch string
	}{
		{"packed", "ref: refs/heads/main\n", "main"},
		{"loose", "ref: refs/heads/main\n", "main"},
		{"detached", head + "\n", "HEAD"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "loose" {
				hosttest.WriteRel(t, repo, ".git/refs/heads/main", head+"\n")
			}
			hosttest.WriteRel(t, repo, ".git/HEAD", tc.head)
			top, branch, skip := CheckoutPeersInfo(context.Background(), repo)
			if top != repo || branch != tc.branch || len(skip) != 1 || skip[0] != nested {
				t.Fatalf("top %q, branch %q, skip %v; want %q, %q, [%s] without Git", top, branch, skip,
					repo, tc.branch, nested)
			}
		})
	}
}

// Both git calls failing leaves the top and yields no branch and no nested dirs.
func TestCheckoutPeersInfoKeepsTopWhenGitFails(t *testing.T) {
	dir := paths.Real(t.TempDir())
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: "+filepath.Join(dir, "missing")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if top, branch, nested := CheckoutPeersInfo(ctx, dir); top != dir || branch != "" || len(nested) != 0 {
		t.Fatalf("top %q, branch %q, nested %v; want top %q and nothing else", top, branch, nested, dir)
	}
}
