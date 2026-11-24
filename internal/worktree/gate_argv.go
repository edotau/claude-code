package worktree

import (
	"strings"

	"github.com/edotau/claude-code/internal/plans"
	"github.com/edotau/claude-code/internal/textutil"
)

// lintBaseRef is the one shell expansion lintVerb writes; the argv run puts the pinned base (or HEAD) in its place.
const lintBaseRef = `"${` + mergeBaseEnv + `:-HEAD}"`

// verifySteps is cmdStr's argv steps with the lint base filled in; !ok when the line needs a shell.
func verifySteps(cmdStr, base string) ([]plans.VerifyStep, bool) {
	if base == "" {
		base = "HEAD"
	}
	steps, err := plans.VerifyArgv(strings.ReplaceAll(cmdStr, lintBaseRef, textutil.ShellQuote(base)))
	return steps, err == nil
}
