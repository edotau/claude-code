package history

// One exec seam for the store. The environment handling below must never drift: getting it wrong is
// silently destructive (a retargeted repo gets `add -A` + `commit` run against it).

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// gitEnv is the process environment with every GIT_* variable removed. Without this an inherited
// GIT_DIR, GIT_WORK_TREE or GIT_INDEX_FILE OVERRIDES the -C flag below, retargeting the store to
// whatever repo the caller's environment happened to name.
func gitEnv() []string {
	env := os.Environ()
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, "GIT_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// gitTimeout bounds one shadow-history git call; a var so tests need not wait it out.
var gitTimeout = 30 * time.Second

// runGit executes git with the sanitized environment. full is the complete argv after "git".
// Only TRAILING newlines are stripped: `status --porcelain` puts a status column in byte 0 and an
// unstaged row begins with a SPACE, so trimming leading whitespace shifts the first row's parse by
// one column — reading an unstaged file as staged, under a path missing its first character.
func runGit(full []string) (string, error) {
	// A tree walk this size is seconds at most; a hung index.lock or filesystem must not hold a hook.
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = gitEnv()
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		// The full argv, not just the subcommand: a wrong --git-dir or -C IS the usual failure here,
		// and the message is the only place it becomes visible.
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(full, " "), err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimRight(out.String(), "\n"), nil
}
