package launch

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
)

// scienceWipePrefixes are Claude Code / harness namespaces: a live session's IPC tokens and routing ride them.
// ANTHROPIC_ is exact-match only (wipeKeys): claude-science reads its own ANTHROPIC_* SSO/BYOK knobs.
var scienceWipePrefixes = []string{"CLAUDE_", "CLAUDECODE", "HARNESS_", "GEMINI_"}

// scienceWiped reports whether key must not reach claude-science, which signs in with its own OAuth.
func scienceWiped(key string) bool {
	if strings.HasPrefix(key, "CLAUDE_SCIENCE_") {
		return false
	}
	if slices.Contains(wipeKeys, key) {
		return true
	}
	return slices.ContainsFunc(scienceWipePrefixes, func(p string) bool { return strings.HasPrefix(key, p) })
}

// Science resolves `claude-code science`: the real claude-science binary, minus the harness env that
// would send its login to the router or gateway (HTTP 401). args pass through untouched.
func Science(env, args []string) (Plan, error) {
	bin, err := paths.LookPathReal("claude-science")
	if err != nil {
		return Plan{}, fmt.Errorf("claude-science not found on PATH; install Claude for Life Sciences from https://claude.ai/download")
	}
	plan := Plan{Binary: bin, Set: map[string]string{}, Argv: args}
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); scienceWiped(k) {
			plan.Unset = append(plan.Unset, k)
		}
	}
	return plan, nil
}

// ScienceEnv is Science over the live environment.
func ScienceEnv(args []string) (Plan, error) { return Science(os.Environ(), args) }
