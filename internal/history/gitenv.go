package history

// One exec seam for the store, via internal/proc. The environment handling there must never
// drift: getting it wrong is silently destructive (a retargeted repo gets `add -A` + `commit` run
// against it).

import (
	"context"
	"time"

	"github.com/edotau/claude-code/internal/proc"
)

// gitTimeout bounds one shadow-history git call; a var so tests need not wait it out.
var gitTimeout = 30 * time.Second

// runGit executes git via the shared proc seam. full is the complete argv after "git",
// already carrying its own -C: dir stays "" here so proc does not add a second one.
func runGit(full []string) (string, error) {
	// A tree walk this size is seconds at most; a hung index.lock or filesystem must not hold a hook.
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	return proc.RunGit(ctx, "", full...)
}
