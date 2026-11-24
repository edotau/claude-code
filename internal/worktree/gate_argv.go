package worktree

import (
	"os/exec"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/plans"
	"github.com/edotau/claude-code/internal/textutil"
)

// lintBaseArg is the one shell expansion lintVerb writes; the argv run puts the pinned base (or HEAD) in its place.
const lintBaseArg = `--base "${` + mergeBaseEnv + `:-HEAD}"`

// verifySteps is cmdStr's argv steps with the lint base filled in; !ok when the line needs a shell.
func verifySteps(cmdStr, base string) ([]plans.VerifyStep, bool) {
	if base == "" {
		base = "HEAD"
	}
	steps, err := plans.VerifyArgv(strings.ReplaceAll(cmdStr, lintBaseArg, "--base "+textutil.ShellQuote(base)))
	if err != nil {
		return nil, false
	}
	for i, s := range steps {
		if s.Argv[0] == "rtk" { // bash pins an explicit rtk to the configured binary
			steps[i].Argv = append([]string{paths.RTKBinary()}, s.Argv[1:]...)
		} else if _, err := exec.LookPath(s.Argv[0]); err != nil && !strings.Contains(s.Argv[0], "/") {
			return nil, false // a builtin or an assignment prefix: only a shell runs it
		}
	}
	return steps, true
}
