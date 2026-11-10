package providers

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
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

func TestConfigFixDatabricksCommandsPreserveExitStatus(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "databricks"), "#!/bin/sh\nprintf '%s' '{\"access_token\":\"fixture-token\"}'\nexit \"$DATABRICKS_TEST_STATUS\"\n", 0o700)
	// This jq stand-in succeeds on the fixture even if databricks printed it before failing.
	writeTestFile(t, filepath.Join(dir, "jq"), `#!/bin/sh
[ "$*" = '-r .access_token' ] || exit 2
IFS= read -r input
[ "$input" = '{"access_token":"fixture-token"}' ] || exit 3
printf '%s\n' 'fixture-token'
`, 0o700)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	command, refresh := DatabricksCLICommands("work")
	for _, tc := range []struct{ name, command string }{{"normal", command}, {"refresh", refresh}} {
		t.Run(tc.name, func(t *testing.T) {
			checkConfigFixCLIStatus(t, tc.command, "23", true)
			checkConfigFixCLIStatus(t, tc.command, "0", false)
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
