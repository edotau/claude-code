package worktree

import (
	"os/exec"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/plans"
	"github.com/edotau/claude-code/internal/textutil"
)

// lintBaseArg is the one shell expansion lintVerb writes; the argv run puts the pinned base (or HEAD) in its place.
const lintBaseArg = `--base "${` + mergeBaseEnv + `:-HEAD}"`

// shellBuiltins are command words bash runs itself, even when an executable of that name exists.
var shellBuiltins = []string{"cd", "export", "source", ".", "eval", "exec", "set", "unset", "alias", "umask", "ulimit",
	"read", "printf", "echo", "test", "[", "true", "false", "pwd", "type", "wait", "trap", "exit", "rtk"}

// NeedsShell is true when only bash reads a step's command word right: a builtin, an assignment prefix, or rtk.
func NeedsShell(word string) bool {
	if slices.Contains(shellBuiltins, word) {
		return true
	}
	_, err := exec.LookPath(word)
	return err != nil && !strings.Contains(word, "/")
}

// verifySteps is cmdStr's argv steps with the lint base filled in; !ok when the line needs a shell.
func verifySteps(cmdStr, base string) ([]plans.VerifyStep, bool) {
	if base == "" {
		base = "HEAD"
	}
	steps, err := plans.VerifyArgv(strings.ReplaceAll(cmdStr, lintBaseArg, "--base "+textutil.ShellQuote(base)))
	if err != nil || slices.ContainsFunc(steps, func(s plans.VerifyStep) bool { return s.Argv[0] != "rtk" && NeedsShell(s.Argv[0]) }) {
		return nil, false
	}
	for i, s := range steps {
		if s.Argv[0] == "rtk" { // bash pins an explicit rtk to the configured binary
			steps[i].Argv = append([]string{paths.RTKBinary()}, s.Argv[1:]...)
		}
	}
	return steps, true
}
