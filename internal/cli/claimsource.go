package cli

import (
	"os"

	"github.com/edotau/claude-code/internal/hooks/wthook"
	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/worktree"
)

// callerClaims is the claim lookup `worktree commit` builds: this session is self, any other live one a peer.
func callerClaims() worktree.ClaimSource {
	return worktree.ClaimSource{Alive: proc.Alive, Self: worktree.SessionSlug(os.Getenv(memory.SessionIDEnv)), Owner: inflight.SessionOwner,
		Label: func(pid int) string { return wthook.SessionLabel(worktree.Proc{PID: pid, Comm: "claude"}) }}
}
