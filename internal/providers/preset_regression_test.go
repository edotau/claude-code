package providers

import (
	"fmt"
	"os/exec"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
)

func TestConfigFixRejectsRoutingEnvOverrides(t *testing.T) {
	for _, key := range []string{"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_GATEWAY"} {
		t.Run(key, func(t *testing.T) {
			errs := validateEnv("dbx", map[string]string{key: "1"})
			want := fmt.Sprintf("dbx: env key %q is owned by the harness", key)
			if len(errs) != 1 || errs[0] != want {
				t.Errorf("validateEnv = %v, want [%s]", errs, want)
			}
		})
	}
}

// Shell functions stand in for the CLIs: a freshly written script costs 60-400 ms on its first macOS exec.
const fakeCLIs = `databricks() { printf '%s' '{"access_token":"fixture-token"}'; return "$DATABRICKS_TEST_STATUS"; }
jq() {
	[ "$*" = '-r .access_token' ] || return 2
	IFS= read -r input
	[ "$input" = '{"access_token":"fixture-token"}' ] || return 3
	printf '%s\n' 'fixture-token'
}
`

func TestConfigFixDatabricksCommandsPreserveExitStatus(t *testing.T) {
	hosttest.SkipOnWindows(t, "sh scripts, symlinks or errno")
	command, refresh := DatabricksCLICommands("work")
	for _, tc := range []struct{ name, command string }{{"normal", command}, {"refresh", refresh}} {
		t.Run(tc.name, func(t *testing.T) {
			checkConfigFixCLIStatus(t, fakeCLIs+tc.command, "23", true)
			checkConfigFixCLIStatus(t, fakeCLIs+tc.command, "0", false)
		})
	}
}

func checkConfigFixCLIStatus(t *testing.T, command, status string, wantFailure bool) {
	t.Helper()
	t.Setenv("DATABRICKS_TEST_STATUS", status)
	out, err := exec.Command("/bin/sh", "-c", command).CombinedOutput()
	if wantFailure {
		exit, ok := err.(*exec.ExitError)
		if !ok || exit.ExitCode() != 23 {
			t.Errorf("upstream failure: err = %v, output = %q; want exit 23", err, out)
		}
		return
	}
	if err != nil || string(out) != "fixture-token\n" {
		t.Errorf("successful token extraction: output = %q, err = %v", out, err)
	}
}
