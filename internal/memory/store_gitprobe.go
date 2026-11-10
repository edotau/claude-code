package memory

import (
	"context"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/proc"
)

// gitTimeout bounds one git call; a var so tests need not wait it out.
var gitTimeout = 30 * time.Second

// gitOut runs `git -C dir args` with the sanitized env and gitTimeout; for identity probes of OTHER repos only.
func gitOut(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	return proc.RunGit(ctx, "", append([]string{"-C", dir}, args...)...)
}

// GitToplevel is dir's git work-tree root, "" outside a repo.
func GitToplevel(dir string) string {
	out, err := gitOut(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}
