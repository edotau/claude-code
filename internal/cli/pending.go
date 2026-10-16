package cli

import "os"

// cmdSecretsOrStub dispatches `secrets` once internal/cli/secrets.go lands (it registers via secretsRun).
func cmdSecretsOrStub(args []string) int {
	if secretsRun != nil {
		return secretsRun(args)
	}
	return fail("secrets: not built into this binary yet (see docs/plans/2026-09-26-claude-dbx-feature-port.md, item 18)")
}

// cmdConfigureOrStub dispatches `configure` once internal/cli/configure.go lands (it registers via configureRun).
func cmdConfigureOrStub(args []string) int {
	if configureRun != nil {
		return configureRun(args)
	}
	_, _ = os.Stderr.WriteString("configure: not built yet — until then: `claude-code switch <provider>`, a key in env.d/secrets.env, `claude-code doctor`.\n")
	return 1
}

// Seams the pending verbs' files assign in an init(); nil until they exist.
var (
	secretsRun   func(args []string) int
	configureRun func(args []string) int
)
