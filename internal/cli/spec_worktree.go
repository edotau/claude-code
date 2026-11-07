package cli

import (
	"os"

	"github.com/edotau/claude-code/internal/worktree"
)

func worktreeResolveAgents() []string { return []string{"claude", "codex"} }

func worktreeNames() []string {
	cwd, err := os.Getwd()
	if err != nil {
		return nil
	}
	return worktree.Names(cwd)
}
