package worktree

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCleanAllDoesNotAliasNestedWorktree(t *testing.T) {
	repo := testRepo(t)
	sibling, err := Create(repo, "feature", CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(repo.Root, "group", "feature")
	commitIn(t, repo.Main, "worktree", "add", "-b", "nested-feature", nested)
	results, err := CleanAll(repo, false)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, result := range results {
		if result.Path == nested {
			found = true
			if result.Removed || !strings.Contains(result.Reason, "nested below") {
				t.Fatalf("nested worktree must be refused under its own path: %+v", result)
			}
		}
	}
	if !found {
		t.Fatalf("nested worktree was aliased to sibling %s: %+v", sibling, results)
	}
	if _, err := os.Stat(nested); err != nil {
		t.Fatal(err)
	}
}
