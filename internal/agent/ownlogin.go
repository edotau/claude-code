package agent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"slices"

	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// cloudForm names the gated form of a vendor argv that submits to the vendor's cloud, else "".
func cloudForm(name string, argv []string) string {
	switch {
	case name == "claude" && slices.Contains(argv, "ultrareview"):
		return `claude-code claude review [--base B|--pr N] --overlay '{"cloud":true}'`
	case name == "codex":
		for i := 1; i < len(argv); i++ {
			if argv[i-1] == "cloud" && argv[i] == "exec" {
				return `claude-code codex cloud exec <query> --env ID --overlay '{"cloud":true}'`
			}
		}
	}
	return ""
}

// RunOwnLogin runs a vendor CLI in the cwd on login's own sign-in only (other credentials and unset names dropped).
func RunOwnLogin(ctx context.Context, login, bin string, unset, argv []string) int {
	dir, err := os.Getwd()
	if err != nil {
		return helptext.Fail("%s: %v", login, err)
	}
	ctx = ProbeContext(ctx, login)
	cmd := exec.CommandContext(ctx, bin, argv...)
	own := map[string]bool{}
	for _, n := range proc.OwnLoginEnv[login] {
		own[n] = true
	}
	env := EnvSpec{Mode: EnvAgent, Unset: unset, PWD: dir}.Environ(ctx)
	// no HARNESS_KEEP_ENV exception applies: every credential-shaped name goes except the login's own
	cmd.Env = proc.Drop(env, own, proc.ScrubDenyCredentials)
	cmd.Dir, cmd.Stdin, cmd.Stdout, cmd.Stderr = dir, os.Stdin, os.Stdout, os.Stderr
	err = cmd.Run()
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		return ee.ExitCode()
	}
	if err != nil {
		return helptext.Fail("%s: %v", login, err)
	}
	return 0
}
