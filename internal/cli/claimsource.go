package cli

import (
	"os"

	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/worktree"
)

// callerClaims is the claim lookup `worktree commit` builds: this session is self, any other live one a peer.
func callerClaims() worktree.ClaimSource {
	return worktree.ClaimSource{Alive: proc.Alive, Self: worktree.SessionSlug(os.Getenv(paths.SessionIDEnv)), Owner: inflight.SessionOwner,
		Label: func(pid int) string { return worktree.SessionLabel(worktree.Proc{PID: pid, Comm: "claude"}) }}
}
