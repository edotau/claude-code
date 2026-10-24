// Package gitexec is the one exec seam for shelling out to git: sanitized environment, caller-supplied
// timeout via ctx, and output trimmed only on the right (porcelain output carries meaning in column 0).
package gitexec

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Env is the process environment with every GIT_* variable removed. Without this an inherited
// GIT_DIR, GIT_WORK_TREE or GIT_INDEX_FILE OVERRIDES a -C flag, retargeting the command to
// whatever repo the caller's environment happened to name.
func Env() []string {
	env := os.Environ()
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if !strings.HasPrefix(kv, "GIT_") {
			out = append(out, kv)
		}
	}
	return out
}

// RunGit executes git with the sanitized environment. dir is passed as -C when non-empty; pass ""
// when args already carries its own -C (or other global flags) ahead of the subcommand.
func RunGit(ctx context.Context, dir string, args ...string) (string, error) {
	full := args
	if dir != "" {
		full = append([]string{"-C", dir}, args...)
	}
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = Env()
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		// The full argv, not just the subcommand: a wrong --git-dir or -C IS the usual failure here,
		// and the message is the only place it becomes visible.
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(full, " "), err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimRight(out.String(), "\n"), nil
}
