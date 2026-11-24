package worktree

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/plans"
	"github.com/edotau/claude-code/internal/rtkwrap"
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

// fillLintBase replaces lintBaseArg outside single quotes with the quoted base.
func fillLintBase(s, base string) string {
	var b strings.Builder
	quoted := false
	for i := 0; i < len(s); {
		switch {
		case !quoted && strings.HasPrefix(s[i:], lintBaseArg):
			b.WriteString("--base " + textutil.ShellQuote(base))
			i += len(lintBaseArg)
			continue
		case s[i] == '\'':
			quoted = !quoted
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// runArgvStep runs one argv step; an exec-format error (a script without a shebang) reruns it under bash as before.
func runArgvStep(ctx context.Context, argv []string, run func(*exec.Cmd) error) error {
	err := run(exec.CommandContext(ctx, argv[0], argv[1:]...))
	if !errors.Is(err, syscall.ENOEXEC) {
		return err
	}
	return run(exec.CommandContext(ctx, "bash", "-c", textutil.ShellJoin(argv)))
}

// verifySteps is cmdStr's argv steps with the lint base filled in; !ok when the line needs a shell.
func verifySteps(cmdStr, base string) ([]plans.VerifyStep, bool) {
	if base == "" {
		base = "HEAD"
	}
	steps, err := plans.VerifyArgv(fillLintBase(cmdStr, base))
	pin := func(w string) bool { return w == "rtk" && os.Getenv(rtkwrap.CodemodeEnv) == "" } // code mode: the shim strips rtk
	if err != nil || slices.ContainsFunc(steps, func(s plans.VerifyStep) bool { return !pin(s.Argv[0]) && NeedsShell(s.Argv[0]) }) {
		return nil, false
	}
	for i, s := range steps {
		if pin(s.Argv[0]) { // bash pins an explicit rtk to the configured binary
			steps[i].Argv = append([]string{paths.RTKBinary()}, s.Argv[1:]...)
		}
	}
	return steps, true
}
