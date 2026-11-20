package proc

import (
	"context"
	"errors"
	"os/exec"
	"strings"
)

// Git runs git in dir with GIT_* scrubbed: ok=false when git itself refused; err for a missing binary or a timeout.
func Git(ctx context.Context, dir string, args ...string) (lines []string, ok bool, err error) {
	out, err := RunGit(ctx, dir, args...)
	var exit *exec.ExitError
	switch {
	case ctx.Err() != nil:
		return nil, false, ctx.Err()
	case errors.As(err, &exit):
		return nil, false, nil
	case err != nil:
		return nil, false, err
	}
	return strings.Split(strings.TrimSpace(out), "\n"), true, nil
}
