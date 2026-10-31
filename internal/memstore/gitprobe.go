package memstore

import (
	"context"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/proc"
)

// GitTimeout bounds one git call; a var so tests need not wait it out.
var GitTimeout = 30 * time.Second

// GitOut runs `git -C dir args` with the sanitized env and GitTimeout; for identity probes of OTHER repos only.
func GitOut(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), GitTimeout)
	defer cancel()
	return proc.RunGit(ctx, "", append([]string{"-C", dir}, args...)...)
}

// GitToplevel is dir's git work-tree root, "" outside a repo.
func GitToplevel(dir string) string {
	out, err := GitOut(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}
