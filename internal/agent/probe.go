// probe.go — the shared run-and-classify helpers for a vendor CLI's sign-in and version probes.
package agent

import (
	"context"
	"errors"
	"os/exec"
	"strings"

	"github.com/edotau/claude-code/internal/providers"
)

// ProbeCLI runs bin args on spec's environment; combined folds stderr into the returned output.
func ProbeCLI(ctx context.Context, bin string, spec EnvSpec, combined bool, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = spec.Environ(ctx)
	if combined {
		return cmd.CombinedOutput()
	}
	return cmd.Output()
}

// ClassifyProbe maps a failed probe: a deadline is transient, an exit the CLI chose is NeedsLogin, else transient.
func ClassifyProbe(ctx context.Context, err error) (providers.AuthVerdict, string) {
	if ctx.Err() != nil {
		return providers.VerdictTransient, ctx.Err().Error()
	}
	if ee, ok := errors.AsType[*exec.ExitError](err); ok && ee.ExitCode() >= 0 {
		return providers.VerdictNeedsLogin, ""
	}
	return providers.VerdictTransient, ProbeErrDetail(err)
}

// probeDetailMax caps the stderr line ProbeErrDetail appends.
const probeDetailMax = 200

// ProbeErrDetail is err plus the first stderr line (≤200 bytes), when the command reported one.
func ProbeErrDetail(err error) string {
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		if line, _, _ := strings.Cut(strings.TrimSpace(string(ee.Stderr)), "\n"); line != "" {
			if len(line) > probeDetailMax {
				line = line[:probeDetailMax]
			}
			return err.Error() + ": " + line
		}
	}
	return err.Error()
}
