package agent

import (
	"cmp"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/textutil"
)

// VersionPolicy is what a CLI major.minor drift from the verified version does; "" is Warn.
type VersionPolicy string

const (
	VersionWarn    VersionPolicy = "warn"
	VersionRequire VersionPolicy = "require"
	VersionIgnore  VersionPolicy = "ignore"
)

// cliProbeTimeout bounds the `--version` probe a run's preflight makes.
const cliProbeTimeout = 2 * time.Second

// CLIVersionAt is the first dotted version `path --version` prints, else ""; tools swaps in its cached probe.
var CLIVersionAt = probeCLIVersion

// probeCLIVersion runs `path --version` under the probe deadline.
func probeCLIVersion(ctx context.Context, path string) string {
	ctx, cancel := context.WithTimeout(ctx, cliProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	cmd.Env = EnvSpec{Mode: EnvAgent}.Environ(ProbeContext(ctx, "")) // a version probe gets no credential
	cmd.WaitDelay = 200 * time.Millisecond
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return textutil.FirstVersion(string(out))
}

// SameMinor reports whether two versions share major.minor; an unparseable one counts as the same.
func SameMinor(a, b string) bool {
	x, y := minorOf(a), minorOf(b)
	return x == "" || y == "" || x == y
}

func minorOf(v string) string {
	parts := strings.Split(textutil.FirstVersion(v), ".")
	if len(parts) < 2 {
		return ""
	}
	return parts[0] + "." + parts[1]
}

// CLIVersionDrift is the drift note and policy for the binary a run of name execs; "" when none.
func CLIVersionDrift(ctx context.Context, name string) (note string, policy VersionPolicy) {
	s, ok := LookupSchema(name)
	if !ok || s.CLI.Verified == "" || s.CLI.VersionPolicy == VersionIgnore {
		return "", cmp.Or(s.CLI.VersionPolicy, VersionWarn)
	}
	bin, err := paths.AgentBin(s.CLI.Binary)
	if err != nil {
		return "", cmp.Or(s.CLI.VersionPolicy, VersionWarn)
	}
	return driftNote(name, CLIVersionAt(ctx, bin), s.CLI)
}

// driftNote is the note and policy (default Warn) for a CLI at version have; "" when it matches or is unknown.
func driftNote(name, have string, c CLI) (string, VersionPolicy) {
	policy := cmp.Or(c.VersionPolicy, VersionWarn)
	if policy == VersionIgnore || SameMinor(have, c.Verified) {
		return "", policy
	}
	return fmt.Sprintf("%s: CLI %s differs from verified %s (%s)", name, have, c.Verified, policy), policy
}
