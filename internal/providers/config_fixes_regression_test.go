package providers

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestConfigFixEmptyPinClearsAcrossProfiles(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(EnvSessionProfile, "current")
	vaulttest.UseKeyring(t)
	reg := &Registry{Default: "current", Providers: map[string]*Provider{
		"current": {Name: "current"}, "other": {Name: "other"},
	}}
	before := vault.AgentRecord{Profile: "current", Model: "all-model", Aliases: map[string]string{
		"opus": "old-opus", "sonnet": "keep-sonnet",
	}}
	want := vault.AgentRecord{Profile: "current", Model: "all-model", Aliases: map[string]string{"sonnet": "keep-sonnet"}}
	for _, profile := range []string{"", "current", "other"} {
		t.Run("profile="+profile, func(t *testing.T) {
			set, err := ParseModelPins(reg, profile, false, []string{"opus="})
			if err != nil {
				t.Fatal(err)
			}
			if spec, ok := set["opus"]; !ok || spec != "" {
				t.Fatalf("parsed opus pin = %q, present %v; want empty unpin", spec, ok)
			}
			if got := WithClaudeModels(before, set); !reflect.DeepEqual(got, want) {
				t.Errorf("record after opus= = %+v, want %+v", got, want)
			}
		})
	}
}

func TestConfigFixArchiveListingErrorIsFinding(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	// A regular file instead of a directory fails portably, even when tests run as root.
	state := filepath.Join(dir, "state")
	writeTestFile(t, state, "not a directory", 0o600)
	for _, row := range checkArchivePerms() {
		if row.Name == "vault archive perms" && strings.Contains(row.Detail, "could not list the vault directory") {
			if row.OK || !row.Gate || !strings.Contains(row.Detail, state) {
				t.Fatalf("listing error must fail the gate and identify its path: %+v", row)
			}
			return
		}
	}
	t.Fatal("archive audit omitted the vault directory listing error")
}

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

func TestConfigFixRecordLastProfileWarnsOnPersistenceFailure(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	state := filepath.Join(dir, "state")
	writeTestFile(t, state, "not a directory", 0o600)
	stderr := captureConfigFixStderr(t, func() { RecordLastProfile("claude", "work") })
	for _, want := range []string{"warning: last profile for claude not saved:", state, "not a directory"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr = %q, want containing %q", stderr, want)
		}
	}
}

func captureConfigFixStderr(t *testing.T, fn func()) string {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	old := os.Stderr
	os.Stderr = file
	defer func() { os.Stderr = old }()
	fn()
	data, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
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
